package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/work"
)

const serveUsage = `usage: factory serve [flags]

Serve the HTTP API until interrupted, then shut down gracefully.
Refuses to start against a database that is behind this binary's migrations.

Flags (each also read from FACTORY_<FLAG>, e.g. FACTORY_ADDR):
  --addr ADDR          listen address (default 127.0.0.1:8080)
  --database-url URL   Postgres URL (env FACTORY_DATABASE_URL)
  --lease DURATION     how long a claim lasts without a heartbeat; a worker
                       silent this long is marked gone (default 5m)
  --stuck-after DURATION
                       revoke a claim with no stage report for this long and
                       count an attempt (default 3h)
  --max-attempts N     failed attempts before a step or item is failed
                       (default 3)
  --retry-delay DURATION
                       wait before handing out a retried step or item
                       (default 1m)
  --github-app-id ID   GitHub App that mints workers' repository tokens; set
                       it with --github-app-key-file, or leave both unset
  --github-app-key-file PATH
                       the GitHub App's PEM private key
  --github-api-url URL GitHub REST API base URL (default https://api.github.com)

Environment:
  FACTORY_API_TOKEN    bearer token required on /api/v1. When unset the API is
                       unauthenticated and --addr must be a loopback address.
`

const (
	shutdownTimeout = 10 * time.Second
	apiTokenEnv     = "FACTORY_API_TOKEN"
)

func serveMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("factory serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "127.0.0.1:8080", "")
	databaseURL := fs.String("database-url", "", "")
	defaults := lease.DefaultPolicy()
	leaseFor := fs.Duration("lease", defaults.Lease, "")
	stuckAfter := fs.Duration("stuck-after", defaults.StuckAfter, "")
	maxAttempts := fs.Int("max-attempts", defaults.MaxAttempts, "")
	retryDelay := fs.Duration("retry-delay", defaults.RetryDelay, "")
	appID := fs.String("github-app-id", "", "")
	appKeyFile := fs.String("github-app-key-file", "", "")
	githubAPI := fs.String("github-api-url", codehost.DefaultGitHubAPI, "")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, serveUsage)
			return 0
		}
		fmt.Fprintf(stderr, "factory serve: %v\n%s", err, serveUsage)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "factory serve: unexpected argument %q\n%s", fs.Arg(0), serveUsage)
		return 2
	}
	policy := lease.Policy{Lease: *leaseFor, StuckAfter: *stuckAfter, MaxAttempts: *maxAttempts, RetryDelay: *retryDelay}
	if err := policy.Validate(); err != nil {
		fmt.Fprintf(stderr, "factory serve: %v\n%s", err, serveUsage)
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
		fmt.Fprintf(stderr, "factory serve: set both --github-app-id and --github-app-key-file to mint repository tokens, or neither.\n%s", serveUsage)
		return 2
	}
	mintToken, err := githubApp(*appID, *appKeyFile, *githubAPI)
	if err != nil {
		fmt.Fprintf(stderr, "factory serve: %v\n", err)
		return 1
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
	svc := work.New(pool).WithPolicy(policy)
	srv := &http.Server{
		Handler:           api.New(api.Config{Work: svc, Ready: ready, Token: token, Logger: logger, MintToken: mintToken}),
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

// githubApp returns the minter of repository tokens for the GitHub App with
// appID and the private key in keyFile, or nil when no App is configured.
// Errors name the file, never its contents.
func githubApp(appID, keyFile, apiURL string) (func(context.Context, string) (codehost.Token, error), error) {
	if appID == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read --github-app-key-file: %w. Point it at the App's private key (.pem)", err)
	}
	key, err := codehost.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("--github-app-key-file %s: %w. Download a new private key from the App's settings", keyFile, err)
	}
	return codehost.TokenMinter{AppID: appID, Key: key, API: apiURL}.Mint, nil
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
