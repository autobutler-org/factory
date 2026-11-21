package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/harness"
)

func TestVersion(t *testing.T) {
	t.Parallel()
	tests := []struct{ client, server, want string }{
		{"v0.3.0", "v0.3.0", Pass},
		{"v0.3.1", "v0.3.0-4-gabc123", Pass},
		{"v0.4.0", "v0.3.2", Fail},
		{"v1.3.0", "v2.3.0", Fail},
		{"dev", "v0.3.0", Skip},
		{"v0.3.0", "abc1234-dirty", Skip},
		{"dev", "dev", Pass},
	}
	for _, tt := range tests {
		r := Version(tt.client, tt.server)
		if r.Status != tt.want || (r.Status == Fail && r.Fix == "") {
			t.Errorf("Version(%q, %q) = %+v, want %s (with a fix when it fails)", tt.client, tt.server, r, tt.want)
		}
	}
}

func TestServerChecks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	count := func(n int, err error) func(context.Context) (int, error) {
		return func(context.Context) (int, error) { return n, err }
	}
	ready := func(err error) func(context.Context) error { return func(context.Context) error { return err } }
	tests := []struct {
		name        string
		got         Result
		wantStatus  string
		wantCommand string
	}{
		{"database ok", Database(ctx, ready(nil)), Pass, ""},
		{"database behind", Database(ctx, ready(fmt.Errorf("check: %w", db.ErrMigrationsPending))), Fail, "factory migrate up"},
		{"database down", Database(ctx, ready(errors.New("database unreachable"))), Fail, ""},
		{"no app", CodeHost(ctx, nil), Fail, ""},
		{"app refused", CodeHost(ctx, count(0, errors.New("401 bad credentials"))), Fail, ""},
		{"app installed nowhere", CodeHost(ctx, count(0, nil)), Fail, ""},
		{"app ok", CodeHost(ctx, count(2, nil)), Pass, ""},
		{"launcher off", Sandbox(ctx, nil), Skip, ""},
		{"cluster unreachable", Sandbox(ctx, count(0, errors.New("forbidden"))), Fail, ""},
		{"cluster ok", Sandbox(ctx, count(0, nil)), Pass, ""},
		{"no worker", Workers(ctx, count(0, nil)), Fail, ""},
		{"workers unknown", Workers(ctx, count(0, errors.New("db down"))), Fail, ""},
		{"a worker", Workers(ctx, count(1, nil)), Pass, ""},
	}
	for _, tt := range tests {
		if tt.got.Status != tt.wantStatus || tt.got.Command != tt.wantCommand {
			t.Errorf("%s = %+v, want %s with command %q", tt.name, tt.got, tt.wantStatus, tt.wantCommand)
		}
		if tt.got.Status == Fail && (tt.got.Fix == "" || tt.got.Detail == "") {
			t.Errorf("%s fails without a detail and a fix: %+v", tt.name, tt.got)
		}
	}
	results := []Result{tt(Pass, "a"), tt(Fail, "b"), tt(Skip, "c"), tt(Fail, "d")}
	if got := Failed(results); !slices.Equal(got, []string{"b", "d"}) {
		t.Errorf("Failed = %v, want every failing check", got)
	}
}

func tt(status, name string) Result { return Result{Name: name, Status: status} }

func TestHarnesses(t *testing.T) {
	t.Parallel()
	ok := &harness.Fake{HarnessName: "claude-code"}
	missing := &harness.Fake{HarnessName: "grok", CheckErr: errors.New("grok was not found")}
	got := Harnesses(t.Context(), []harness.Harness{ok, missing})
	if got[0].Status != Pass || got[1].Status != Skip {
		t.Errorf("one ready = %+v, want pass and skip", got)
	}
	got = Harnesses(t.Context(), []harness.Harness{missing})
	if got[0].Status != Fail || got[0].Fix == "" {
		t.Errorf("none ready = %+v, want a failure with a fix", got)
	}
}

func TestGitAndWorkspace(t *testing.T) {
	t.Parallel()
	if r := Git(t.Context()); r.Status != Pass {
		t.Errorf("Git = %+v, want pass where the tests run", r)
	}
	dir := filepath.Join(t.TempDir(), "work")
	if r := Workspace(dir); r.Status != Pass {
		t.Errorf("Workspace(new dir) = %+v, want pass", r)
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	if r := Workspace(filepath.Join(readOnly, "work")); os.Geteuid() != 0 && (r.Status != Fail || r.Fix == "") {
		t.Errorf("Workspace(read-only parent) = %+v, want a failure with a fix", r)
	}
}
