//go:build e2e

// The end-to-end tests run a real factory API (httptest) on a dbtest
// database, a fake GitHub (fakegh), a local bare repository as origin, and
// the fake agent behind the real Claude Code harness. Run them with
// `make test/e2e`.
package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/codehost/fakegh"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/harness"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/autobutler-org/factory/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	repo    = "o/quark"
	ghToken = "ghs_e2etoken0000000000000000000000000000"
)

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *work.Service
	gh     *fakegh.Server
	api    string
	origin string
	// agentDir holds the fake agent, its wrapper, and the scripts the
	// wrapper picks between: implement and review.
	agentDir string
	marker   string
	// config is the .factory/config.toml on origin's main.
	config string
	item   db.WorkItem
}

// run is a command for the test, bounded by the test's context.
func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func newEnv(t *testing.T, gate string, maxAttempts int) *env {
	t.Helper()
	e := &env{t: t, agentDir: t.TempDir(), origin: t.TempDir()}
	e.marker = filepath.Join(t.TempDir(), "gate-ran")

	e.pool = dbtest.NewDB(t)
	e.svc = work.New(e.pool).WithPolicy(lease.Policy{Lease: time.Minute, StuckAfter: time.Hour, MaxAttempts: maxAttempts, RetryDelay: 0})
	e.gh = fakegh.New(t, repo, ghToken)
	mint := func(context.Context, string) (codehost.Token, error) {
		return codehost.Token{Token: ghToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	srv := httptest.NewServer(api.New(api.Config{
		Work: e.svc, Ready: func(context.Context) error { return nil }, MintToken: mint,
		Logger: slog.New(slog.DiscardHandler),
	}))
	t.Cleanup(srv.Close)
	e.api = srv.URL

	// The origin: o/quark with a gate in .factory/config.toml on main.
	bare := filepath.Join(e.origin, repo+".git")
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatal(err)
	}
	run(t, bare, "git", "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	run(t, seed, "git", "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(seed, ".factory"), 0o750); err != nil {
		t.Fatal(err)
	}
	e.config = fmt.Sprintf("gate = %q\n", strings.ReplaceAll(gate, "MARKER", e.marker))
	if err := os.WriteFile(filepath.Join(seed, ".factory", "config.toml"), []byte(e.config), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "git", "add", "-A")
	run(t, seed, "git", "commit", "-q", "-m", "seed")
	run(t, seed, "git", "push", "-q", bare, "main")

	// The agent: cmd/fakeagent, behind a wrapper that runs the review script
	// for a review prompt and the implement script for any other.
	run(t, ".", "go", "build", "-o", filepath.Join(e.agentDir, "fakeagent"), "github.com/autobutler-org/factory/cmd/fakeagent")
	wrapper := `#!/bin/sh
prompt=$(cat)
case "$prompt" in
"You are reviewing"*) script=review ;;
*) script=implement ;;
esac
FAKEAGENT_SCRIPT=$(cat "` + e.agentDir + `/$script") exec "` + e.agentDir + `/fakeagent" "$@"
`
	if err := os.WriteFile(filepath.Join(e.agentDir, "agent"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	e.script("implement", "stdout working\nedit hello.txt hello\nedit .factory/pr.md Add hello")
	e.script("review", "edit .factory/review.md pass")

	res, err := e.svc.Upsert(t.Context(), work.NewItem{
		Source: "jira", ExternalID: "F-1", ExternalURL: "https://jira.test/browse/F-1",
		Title: "add hello", Body: "Add hello.txt.", Repos: []string{repo},
	}, work.Actor{Kind: work.ActorAdapter, ID: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	e.item = res.Item
	return e
}

func (e *env) script(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.agentDir, name), []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// worker runs one worker process until it exits.
func (e *env) worker(ctx context.Context, name string) error {
	return worker.Run(ctx, worker.Config{
		Client:    &worker.Client{URL: e.api},
		Name:      name,
		WorkDir:   e.t.TempDir(),
		Harnesses: []harness.Harness{&harness.ClaudeCode{Bin: filepath.Join(e.agentDir, "agent")}},
		GitURL:    "file://" + e.origin,
		Host: func(repo string, tokens func(context.Context) (string, error)) codehost.Host {
			return codehost.GitHub{API: e.gh.URL, Repo: repo, Tokens: tokens}
		},
		Output: io.Discard,
		Log:    slog.New(slog.NewTextHandler(e.t.Output(), nil)),
	})
}

func (e *env) state() db.WorkItem {
	e.t.Helper()
	item, err := e.svc.Get(e.t.Context(), e.item.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return item
}

// transitions lists the item's states in order.
func (e *env) transitions() []string {
	e.t.Helper()
	events, err := e.svc.Events(e.t.Context(), e.item.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	states := []string{"ready"}
	for _, ev := range events {
		if ev.Type == work.EventTransition && ev.ToState != nil {
			states = append(states, *ev.ToState)
		}
	}
	return states
}

func (e *env) step(key string) db.WorkStep {
	e.t.Helper()
	g, err := e.svc.Graph(e.t.Context(), e.item.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, s := range g.Steps {
		if s.Key == key {
			return s
		}
	}
	e.t.Fatalf("no step %s", key)
	return db.WorkStep{}
}

// inReview checks the happy path's end: one open pull request recorded, in
// review, the branch pushed, the gate run.
func (e *env) inReview() {
	t := e.t
	t.Helper()
	if got := e.transitions(); !slices.Equal(got, []string{"ready", "in_progress", "in_review"}) {
		t.Errorf("states = %v, want ready, in_progress, in_review", got)
	}
	outputs, err := e.svc.Outputs(t.Context(), e.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	pulls, _ := e.gh.Snapshot()
	if len(pulls) != 1 || len(outputs) != 1 || outputs[0].Url != pulls[0].URL || outputs[0].Status != "open" || outputs[0].Kind != "pull_request" {
		t.Fatalf("pulls %+v, outputs %+v: want one pull request recorded", pulls, outputs)
	}
	p := pulls[0]
	if p.Title != "Add hello" || p.Base != "main" || !strings.Contains(p.Body, "Source: https://jira.test/browse/F-1") || !strings.HasPrefix(p.Head, e.item.ID[:8]) {
		t.Errorf("pull = %+v", p)
	}
	if _, err := os.Stat(e.marker); err != nil {
		t.Errorf("the gate did not run: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), "git", "--git-dir", filepath.Join(e.origin, repo+".git"), "show", p.Head+":hello.txt").Output()
	if err != nil || string(out) != "hello\n" {
		t.Errorf("pushed hello.txt = %q, %v", out, err)
	}
	if s := e.step(repo + "/review-1"); s.State != "succeeded" || !strings.Contains(string(s.Result), `"pass"`) {
		t.Errorf("review-1 = %s %s", s.State, s.Result)
	}
}

func TestE2EOneItemToReview(t *testing.T) {
	e := newEnv(t, "test -f hello.txt && touch MARKER", 3)
	if err := e.worker(t.Context(), "sandbox-1"); err != nil {
		t.Fatal(err)
	}
	e.inReview()
	if s := e.step(repo + "/await_checks-1"); s.State != "ready" {
		t.Errorf("await_checks-1 = %s, want ready for the adapter", s.State)
	}
}

func TestE2EKilledMidStepResumesWithoutASecondPullRequest(t *testing.T) {
	e := newEnv(t, "test -f hello.txt && touch MARKER", 3)
	e.script("review", "hang")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.worker(ctx, "sandbox-1") }()
	deadline := time.Now().Add(time.Minute)
	running := func() bool {
		g, err := e.svc.Graph(t.Context(), e.item.ID)
		return err == nil && slices.ContainsFunc(g.Steps, func(s db.WorkStep) bool { return s.Key == repo+"/review-1" && s.State == "running" })
	}
	for !running() {
		if time.Now().After(deadline) {
			t.Fatal("review-1 never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel() // SIGTERM: the step is released
	if err := <-done; err != nil {
		t.Fatalf("first worker = %v", err)
	}
	if s := e.step(repo + "/review-1"); s.State != "ready" || s.Attempts != 0 {
		t.Fatalf("review-1 after the kill = %s with %d attempts, want ready with none counted", s.State, s.Attempts)
	}

	// A new sandbox with an empty disk takes over.
	e.script("review", "edit .factory/review.md pass")
	if err := e.worker(t.Context(), "sandbox-2"); err != nil {
		t.Fatal(err)
	}
	e.inReview()
}

func TestE2EGateFailureFailsTheItemWithTheOutput(t *testing.T) {
	e := newEnv(t, "echo the gate says no; exit 1", 3)
	if err := e.worker(t.Context(), "sandbox-1"); err != nil {
		t.Fatal(err)
	}
	item := e.state()
	if item.State != "failed" || !strings.Contains(item.StateReason, "the gate says no") {
		t.Fatalf("item = %s %q, want failed with the gate output", item.State, item.StateReason)
	}
	events, _ := e.svc.Events(t.Context(), e.item.ID)
	found := false
	for _, ev := range events {
		var p struct {
			Key    string `json:"key"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		found = found || (ev.Type == work.EventStepResult && p.Key == repo+"/gate" && strings.Contains(p.Reason, "the gate says no"))
	}
	if !found {
		t.Error("no step result event for the gate carries its output")
	}
	if s := e.step(repo + "/gate"); s.Attempts != 1 {
		t.Errorf("gate attempts = %d, want 1: a gate failure is not retried", s.Attempts)
	}
	if pulls, _ := e.gh.Snapshot(); len(pulls) != 0 {
		t.Errorf("pulls = %+v, want none", pulls)
	}
}

func TestE2ECrashIsRetriedUpToTheLimit(t *testing.T) {
	e := newEnv(t, "true", 2)
	e.script("implement", "stderr the agent crashed\nexit 1")
	for i := range 3 {
		if err := e.worker(t.Context(), fmt.Sprintf("sandbox-%d", i)); err != nil {
			t.Fatal(err)
		}
		if e.state().State == "failed" {
			break
		}
	}
	item := e.state()
	if item.State != "failed" || !strings.Contains(item.StateReason, "after 2 attempts") {
		t.Fatalf("item = %s %q, want failed after 2 attempts", item.State, item.StateReason)
	}
	if s := e.step(repo + "/implement"); s.Attempts != 2 {
		t.Errorf("implement attempts = %d, want 2", s.Attempts)
	}
}
