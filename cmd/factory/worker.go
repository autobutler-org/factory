package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/harness"
	"github.com/autobutler-org/factory/internal/worker"
)

const workerUsage = `usage: factory worker [flags]

Run the step executor: register with the factory server, claim steps one at a
time, run each, and report the result. Exits 0 when the server has nothing
left to hand out. On SIGTERM the step in hand is released, which counts no
attempt.

Flags (each also read from FACTORY_<FLAG>, e.g. FACTORY_URL):
  --url URL            the factory server (required)
  --token-file PATH    file holding the bearer token, read on every request
  --sandbox NAME       the worker's name, the sandbox it runs in (default:
                       the host name)
  --sandbox-key KEY    claim only this chain's steps (<item id>/<owner>/<repo>)
  --work-dir DIR       where checkouts live (default $TMPDIR/factory-worker)
  --harnesses LIST     agent CLIs to use, in order (default claude-code,grok);
                       those not installed or not logged in are skipped
  --git-url URL        base of clone URLs, <git-url>/<owner>/<repo>.git
                       (default https://github.com)
  --github-api-url URL GitHub REST API base URL (default https://api.github.com)

Environment:
  FACTORY_API_TOKEN    bearer token when --token-file is not set
`

// harnessCheckTimeout bounds checking that the agent CLIs can run.
const harnessCheckTimeout = 30 * time.Second

func workerMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("factory worker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "")
	tokenFile := fs.String("token-file", "", "")
	name := fs.String("sandbox", "", "")
	sandboxKey := fs.String("sandbox-key", "", "")
	workDir := fs.String("work-dir", filepath.Join(os.TempDir(), "factory-worker"), "")
	harnesses := fs.String("harnesses", "claude-code,grok", "")
	gitURL := fs.String("git-url", "https://github.com", "")
	githubAPI := fs.String("github-api-url", codehost.DefaultGitHubAPI, "")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, workerUsage)
			return 0
		}
		fmt.Fprintf(stderr, "factory worker: %v\n%s", err, workerUsage)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "factory worker: unexpected argument %q\n%s", fs.Arg(0), workerUsage)
		return 2
	}
	if *url == "" {
		fmt.Fprintf(stderr, "factory worker: set --url (or FACTORY_URL) to the factory server, e.g. --url http://127.0.0.1:8080\n%s", workerUsage)
		return 2
	}
	if *name == "" {
		host, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(stderr, "factory worker: no host name (%v); set --sandbox (or FACTORY_SANDBOX)\n", err)
			return 2
		}
		*name = host
	}
	chosen, err := pickHarnesses(*harnesses)
	if err != nil {
		fmt.Fprintf(stderr, "factory worker: %v\n%s", err, workerUsage)
		return 2
	}
	checkCtx, cancel := context.WithTimeout(ctx, harnessCheckTimeout)
	var ready []harness.Harness
	var problems []string
	for _, h := range chosen {
		if err := h.Check(checkCtx); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", h.Name(), err))
			continue
		}
		ready = append(ready, h)
	}
	cancel()
	if len(ready) == 0 {
		fmt.Fprintf(stderr, "factory worker: no agent harness can run:\n  %s\nInstall and log in to one of them, then start the worker again.\n", strings.Join(problems, "\n  "))
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	err = worker.Run(ctx, worker.Config{
		Client:     &worker.Client{URL: *url, Token: bearer(*tokenFile)},
		Name:       *name,
		Version:    version,
		SandboxKey: *sandboxKey,
		WorkDir:    *workDir,
		Harnesses:  ready,
		GitURL:     *gitURL,
		Host: func(repo string, tokens func(context.Context) (string, error)) codehost.Host {
			return codehost.GitHub{API: *githubAPI, Repo: repo, Tokens: tokens}
		},
		Output: stderr,
		Log:    logger,
	})
	if err != nil {
		logger.Error("worker stopped", "err", err)
		return 1
	}
	return 0
}

// pickHarnesses returns the named harnesses, in order.
func pickHarnesses(list string) ([]harness.Harness, error) {
	var out []harness.Harness
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		i := slices.IndexFunc(harness.All(), func(h harness.Harness) bool { return h.Name() == name })
		if i < 0 {
			return nil, fmt.Errorf("--harnesses: unknown harness %q; use claude-code, grok, or both", name)
		}
		out = append(out, harness.All()[i])
	}
	return out, nil
}

// bearer returns the token source: the file, read fresh on every request so
// a token the kubelet rotates is picked up, or FACTORY_API_TOKEN.
func bearer(file string) func() (string, error) {
	if file == "" {
		token := os.Getenv(apiTokenEnv)
		return func() (string, error) { return token, nil }
	}
	return func() (string, error) {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --token-file: %w; check that the token is mounted at %s", err, file)
		}
		return strings.TrimSpace(string(data)), nil
	}
}
