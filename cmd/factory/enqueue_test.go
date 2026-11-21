package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/work"
)

func TestWriteCommands(t *testing.T) {
	const id = "0b8c1f9e-0000-4000-8000-000000000002"
	routes := map[string]reply{
		"POST /api/v1/work":                        {http.StatusCreated, `{"item":` + readyJSON + `,"created":true,"changed":true,"reason":""}`},
		"POST /api/v1/work/" + id + "/transitions": ok(strings.Replace(readyJSON, `"state":"ready"`, `"state":"design"`, 1)),
		"POST /api/v1/work/done/transitions":       {http.StatusConflict, `{"error":{"code":"illegal_transition","message":"illegal transition","current_state":"design","allowed":["ready","blocked","wont_fix"]}}`},
		"POST /api/v1/work/waits/transitions":      apiError(409, "unfinished_dependencies", "an item it is blocked by is unfinished"),
		"POST /api/v1/work/nope/transitions":       apiError(404, "not_found", "work item not found"),
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "item.json")
	if err := os.WriteFile(file, []byte(`{"title": "rotate the staging certificates"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	notJSON := filepath.Join(dir, "item.txt")
	if err := os.WriteFile(notJSON, []byte("title: x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"enqueue":                {"work", "enqueue", "--title", "rotate the staging certificates", "--repos", "autobutler-org/infra"},
		"enqueue_json":           {"work", "enqueue", "--title", "rotate the staging certificates", "--output", "json"},
		"enqueue_file":           {"work", "enqueue", "--file", file},
		"enqueue_no_title":       {"work", "enqueue"},
		"enqueue_bad_state":      {"work", "enqueue", "--title", "x", "--state", "completed"},
		"enqueue_file_and_flags": {"work", "enqueue", "--file", file, "--title", "x"},
		"enqueue_missing_file":   {"work", "enqueue", "--file", "/nonexistent/item.json"},
		"enqueue_not_json":       {"work", "enqueue", "--file", notJSON},
		"move":                   {"work", "move", id, "design", "--reason", "needs a design"},
		"move_json":              {"work", "move", "--output", "json", id, "design"},
		"move_refused":           {"work", "move", "done", "completed"},
		"move_blocked":           {"work", "move", "waits", "in_progress"},
		"move_not_found":         {"work", "move", "nope", "ready"},
		"move_bad_state":         {"work", "move", id, "sideways"},
		"move_no_state":          {"work", "move", id},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fakeServer(t, routes)
			out := transcript(t.Context(), args...)
			golden(t, "write/"+name, strings.ReplaceAll(out, dir, "$TMP"))
		})
	}
}

// TestWriteCommandsAgainstTheAPI runs enqueue and move against the real API,
// so the bodies the CLI sends and the refusals it reads are the server's.
func TestWriteCommandsAgainstTheAPI(t *testing.T) {
	pool := dbtest.NewDB(t)
	srv := httptest.NewServer(api.New(api.Config{Work: work.New(pool), Logger: slog.New(slog.DiscardHandler)}))
	t.Cleanup(srv.Close)
	t.Setenv(serverEnv, srv.URL)
	t.Setenv(apiTokenEnv, "")
	runOK := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
		return stdout.String()
	}
	out := runOK("work", "enqueue", "--title", "an idea", "--state", "design", "--repos", "o/r", "--requires", "claude", "--idempotency-key", "k", "--output", "json")
	var id string
	if _, after, found := strings.Cut(out, `"id": "`); found {
		id, _, _ = strings.Cut(after, `"`)
	}
	if id == "" {
		t.Fatalf("enqueue printed no id: %s", out)
	}
	if again := runOK("work", "enqueue", "--title", "an idea", "--idempotency-key", "k"); !strings.Contains(again, "Already enqueued "+id) {
		t.Errorf("repeat with the same key = %q, want the first item", again)
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"work", "move", id, "completed"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "From design it may move to: ready, blocked, wont_fix.") {
		t.Errorf("illegal move: exit %d, stderr %q; want the allowed targets", code, stderr.String())
	}
	if moved := runOK("work", "move", id, "ready", "--reason", "designed"); moved != "Moved "+id+" to ready.\n" {
		t.Errorf("move = %q", moved)
	}
	if shown := runOK("work", "show", id); !strings.Contains(shown, "transition design -> ready: designed") {
		t.Errorf("show = %q, want the move on the timeline", shown)
	}
}
