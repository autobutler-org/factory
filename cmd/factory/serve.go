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
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/work"
)

const serveUsage = `usage: factory serve [flags]

Serve the HTTP API until interrupted, then shut down gracefully.
Refuses to start against a database that is behind this binary's migrations.

Flags (each also read from FACTORY_<FLAG>, e.g. FACTORY_ADDR):
  --addr ADDR          listen address (default 127.0.0.1:8080)
  --database-url URL   Postgres URL (env FACTORY_DATABASE_URL)

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
	srv := &http.Server{
		Handler:           api.New(api.Config{Work: work.New(pool), Ready: ready, Token: token, Logger: logger}),
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

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
