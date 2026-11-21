package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/doctor"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/server"
	"github.com/autobutler-org/factory/internal/work"
)

const (
	shutdownTimeout = 10 * time.Second
	apiTokenEnv     = "FACTORY_API_TOKEN"
)

func serveCommand() command {
	return command{
		name:    "serve",
		summary: "Serve the HTTP API and the web UI.",
		about: `Serve the HTTP API until interrupted, then shut down gracefully.
Refuses to start against a database that is behind this binary's migrations.
The --launcher flags start worker sandboxes as Kubernetes Jobs and need
in-cluster config.`,
		env: `  FACTORY_API_TOKEN    bearer token required on /api/v1. When unset the API is
                       unauthenticated and --addr must be a loopback address.`,
		fromEnv: true,
		setup:   serveSetup,
	}
}

func serveSetup(fs *flag.FlagSet) runner {
	addr := fs.String("addr", "127.0.0.1:8080", "listen `address`")
	databaseURL := fs.String("database-url", "", "Postgres `URL`")
	defaults := lease.DefaultPolicy()
	leaseFor := fs.Duration("lease", defaults.Lease, "how long a claim lasts without a heartbeat; a worker silent this long is marked gone")
	stuckAfter := fs.Duration("stuck-after", defaults.StuckAfter, "revoke a claim with no stage report for this long and count an attempt")
	maxAttempts := fs.Int("max-attempts", defaults.MaxAttempts, "failed attempts before a step or item is failed")
	retryDelay := fs.Duration("retry-delay", defaults.RetryDelay, "wait before handing out a retried step or item")
	appID := fs.String("github-app-id", "", "GitHub App `ID` that mints workers' repository tokens; set it with --github-app-key-file, or leave both unset")
	appKeyFile := fs.String("github-app-key-file", "", "the GitHub App's PEM private key `file`")
	githubAPI := fs.String("github-api-url", codehost.DefaultGitHubAPI, "GitHub REST API base `URL`")
	launch := addLauncherFlags(fs)
	return func(ctx context.Context, _ []string, s streams) int {
		stderr := s.err
		policy := lease.Policy{Lease: *leaseFor, StuckAfter: *stuckAfter, MaxAttempts: *maxAttempts, RetryDelay: *retryDelay}
		if err := policy.Validate(); err != nil {
			fmt.Fprintf(stderr, "factory serve: %v\nRun 'factory serve --help' for usage.\n", err)
			return 2
		}
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			fmt.Fprintf(stderr, "factory serve: --addr %q is not host:port (%v). Use, for example, --addr 127.0.0.1:8080.\n", *addr, err)
			return 2
		}
		// ponytail: one shared bearer token and no caller identities; #15 adds
		// sign-in, roles and scoped tokens and replaces this.
		token := os.Getenv(apiTokenEnv)
		if token == "" && !isLoopback(host) {
			fmt.Fprintf(stderr, "factory serve: %s is not set, so the API would be unauthenticated on %s. "+
				"Set %s, or bind to loopback with --addr 127.0.0.1:8080.\n", apiTokenEnv, *addr, apiTokenEnv)
			return 2
		}
		if (*appID == "") != (*appKeyFile == "") {
			fmt.Fprintf(stderr, "factory serve: set both --github-app-id and --github-app-key-file to mint repository tokens, or neither.\n")
			return 2
		}
		jobsConfig, err := launch.jobsConfig()
		if err != nil {
			fmt.Fprintf(stderr, "factory serve: %v\nRun 'factory serve --help' for usage.\n", err)
			return 2
		}
		app, commitIdentity, err := githubApp(*appID, *appKeyFile, *githubAPI)
		if err != nil {
			fmt.Fprintf(stderr, "factory serve: %v\n", err)
			return 1
		}
		var jobs *sandbox.Jobs
		if *launch.enabled {
			if jobs, err = newJobs(jobsConfig); err != nil {
				fmt.Fprintf(stderr, "factory serve: %v\n", err)
				return 1
			}
		}
		if *databaseURL == "" {
			fmt.Fprintf(stderr, "factory serve: %v\n", db.ErrNoDatabaseURL)
			return 1
		}

		logger := slog.New(slog.NewJSONHandler(stderr, nil))
		pool, err := db.Open(ctx, *databaseURL)
		if err != nil {
			fmt.Fprintf(stderr, "factory serve: %v\n", err)
			return 1
		}
		defer pool.Close()
		if err := db.CheckMigrated(ctx, pool); err != nil {
			fmt.Fprintf(stderr, "factory serve: %v\n", err)
			return 1
		}

		ready := func(ctx context.Context) error {
			if _, err := db.New(pool).Ping(ctx); err != nil {
				return fmt.Errorf("database unreachable: %w", err)
			}
			return db.CheckMigrated(ctx, pool)
		}
		var mintToken func(context.Context, string) (codehost.Token, error)
		if app != nil {
			mintToken = app.Mint
		}
		svc := work.New(pool).WithPolicy(policy)
		codeHost := githubHost(mintToken, *githubAPI)
		if codeHost != nil {
			// With a GitHub App the server plans each item with its
			// repositories' .factory/config.toml; without one, with defaults.
			svc = svc.WithConfigs(work.Configs{Host: codeHost, AllowedImages: splitList(*launch.allowedImages)})
		} else {
			logger.Warn("no GitHub App is configured: items are planned with default repository settings, " +
				"not their .factory/config.toml. Set --github-app-id and --github-app-key-file to read it")
		}
		if mintToken != nil && commitIdentity == nil {
			logger.Warn("workers commit as factory, not as the GitHub App's bot: the bot's noreply email domain is known "+
				"for github.com only", "github_api_url", *githubAPI)
		}
		apiConfig := api.Config{
			Work: svc, Ready: ready, Token: token, Logger: logger, MintToken: mintToken, CommitIdentity: commitIdentity,
			UI: server.UI(server.Public()), Version: version, Doctor: serverChecks(ready, app, jobs, svc),
		}
		if jobs != nil {
			// Assigned only when set: a nil *Jobs in the interface would not be nil.
			apiConfig.Sandboxes = jobs
		}
		srv := &http.Server{
			Handler:           api.New(apiConfig),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		listener, err := net.Listen("tcp", *addr)
		if err != nil {
			fmt.Fprintf(stderr, "factory serve: listen on %s: %v. Pick a free address with --addr.\n", *addr, err)
			return 1
		}
		if token == "" {
			logger.Warn("FACTORY_API_TOKEN is not set: the API is unauthenticated and bound to loopback only", "addr", listener.Addr().String())
		}
		logger.Info("serving", "addr", listener.Addr().String())

		reaperDone := make(chan struct{})
		go func() {
			defer close(reaperDone)
			reap(ctx, svc, max(policy.Lease/5, time.Second), logger)
		}()
		defer func() { <-reaperDone }()

		// The server's own worker runs server-side steps only with the
		// launcher on, so a server without it behaves as before.
		if jobs != nil {
			l := launch.newLauncher(jobs, svc, pool, codeHost, logger)
			launcherDone := make(chan struct{})
			go func() {
				defer close(launcherDone)
				l.Run(ctx, launcherInterval)
			}()
			defer func() { <-launcherDone }()
			logger.Info("launcher on", "namespace", jobsConfig.Namespace, "image", jobsConfig.Image, "allowed_images", *launch.allowedImages, "max_sandboxes", *launch.maxSandboxes)
		}

		served := make(chan error, 1)
		go func() { served <- srv.Serve(listener) }()
		select {
		case err := <-served:
			logger.Error("server stopped", "err", err)
			return 1
		case <-ctx.Done():
		}
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			return 1
		}
		logger.Info("stopped")
		return 0
	}
}

// serverChecks are the server side of factory doctor. app is nil without a
// GitHub App and jobs nil with the launcher off.
func serverChecks(ready func(context.Context) error, app *codehost.TokenMinter, jobs *sandbox.Jobs, svc *work.Service) func(context.Context) []doctor.Result {
	var installations, sandboxes func(context.Context) (int, error)
	if app != nil {
		installations = app.Installations
	}
	if jobs != nil {
		sandboxes = func(ctx context.Context) (int, error) {
			list, err := jobs.List(ctx)
			return len(list), err
		}
	}
	active := func(ctx context.Context) (int, error) {
		workers, err := svc.Workers(ctx)
		n := 0
		for _, w := range workers {
			if w.Kind == work.WorkerCluster && w.State == work.WorkerActive {
				n++
			}
		}
		return n, err
	}
	return func(ctx context.Context) []doctor.Result {
		return []doctor.Result{
			doctor.Database(ctx, ready),
			doctor.CodeHost(ctx, installations),
			doctor.Sandbox(ctx, sandboxes),
			doctor.Workers(ctx, active),
		}
	}
}

// githubApp returns the GitHub App that mints repository tokens, with
// appID and the private key in keyFile, and the App bot's commit identity,
// looked up on first use and then cached. Both are nil when no App is
// configured, and the identity is nil off github.com, whose noreply domain
// is the only one it knows. Errors name the file, never its contents.
func githubApp(appID, keyFile, apiURL string) (
	app *codehost.TokenMinter, identity func(context.Context) (codehost.Identity, error), err error,
) {
	if appID == "" {
		return nil, nil, nil
	}
	pemBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read --github-app-key-file: %w. Point it at the App's private key (.pem)", err)
	}
	key, err := codehost.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("--github-app-key-file %s: %w. Download a new private key from the App's settings", keyFile, err)
	}
	minter := codehost.TokenMinter{AppID: appID, Key: key, API: apiURL}
	if strings.TrimSuffix(apiURL, "/") == codehost.DefaultGitHubAPI {
		identity = (&codehost.IdentityCache{Lookup: minter.Identity}).Get
	}
	return &minter, identity, nil
}

// reap runs the lease reaper, for item and step leases, every interval until
// ctx ends. Several replicas
// may run it at once; Reap takes rows with SKIP LOCKED.
func reap(ctx context.Context, svc *work.Service, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		report, err := svc.Reap(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("reaper pass failed", "err", err)
		case report != work.ReapReport{}:
			logger.Info("reaper pass", "expired", report.Expired, "stuck", report.Stuck,
				"steps_expired", report.StepsExpired, "steps_stuck", report.StepsStuck, "gone", report.Gone)
		}
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
