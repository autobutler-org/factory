package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden compares got with testdata/<name>.golden. UPDATE_GOLDEN=1 (make
// test/golden) rewrites the file instead.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run `make test/golden` to write it", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file; if the change is intended, run `make test/golden`.\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// transcript runs factory with args and renders the exit code and both
// streams, for a golden file.
func transcript(ctx context.Context, args ...string) string {
	var stdout, stderr bytes.Buffer
	code := run(ctx, args, &stdout, &stderr)
	return fmt.Sprintf("$ factory %s\nexit %d\n--- stdout\n%s--- stderr\n%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
}

// TestHelp pins the usage text of factory itself, every group, and every
// command in the table.
func TestHelp(t *testing.T) {
	cases := map[string][]string{
		"factory":         nil,
		"factory_-h":      {"-h"},
		"factory_--help":  {"--help"},
		"unknown":         {"nope"},
		"unknown_flag":    {"--nope"},
		"migrate":         {"migrate"},
		"migrate_--help":  {"migrate", "--help"},
		"migrate_unknown": {"migrate", "sideways"},
	}
	for _, c := range commands() {
		cases[strings.ReplaceAll(c.name, " ", "_")+"_-h"] = append(strings.Fields(c.name), "-h")
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			golden(t, "help/"+name, transcript(t.Context(), args...))
		})
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version unstamped", args: []string{"version"}, wantStdout: "factory dev (commit none, built unknown)\n"},
		{
			name: "version extra argument", args: []string{"version", "extra"}, wantCode: 2,
			wantStderr: "factory version: unexpected argument \"extra\"\nRun 'factory version --help' for usage.\n",
		},
		{
			name: "bad flag", args: []string{"migrate", "up", "--nope"}, wantCode: 2,
			wantStderr: "factory migrate up: flag provided but not defined: -nope\nRun 'factory migrate up --help' for usage.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}
