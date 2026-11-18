package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRefusesBeforeRegistering(t *testing.T) {
	t.Setenv("FACTORY_URL", "")
	t.Setenv("FACTORY_SANDBOX", "")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "help", args: []string{"--help"}},
		{name: "extra argument", args: []string{"--url", "http://x", "now"}, wantCode: 2, wantStderr: "unexpected argument"},
		{name: "no url", args: nil, wantCode: 2, wantStderr: "set --url"},
		{name: "unknown harness", args: []string{"--url", "http://x", "--harnesses", "gpt"}, wantCode: 2, wantStderr: `unknown harness "gpt"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), append([]string{"worker"}, tt.args...), &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestWorkerSaysWhenNoHarnessCanRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stderr bytes.Buffer
	if got := run(t.Context(), []string{"worker", "--url", "http://127.0.0.1:1"}, &bytes.Buffer{}, &stderr); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "claude-code") || !strings.Contains(stderr.String(), "install") {
		t.Errorf("stderr = %q, want each harness and how to fix it", stderr.String())
	}
}

func TestBearerReadsTheFileOnEveryRequest(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	token := bearer(file)
	if _, err := token(); err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("missing file: %v, want an error naming it", err)
	}
	for _, want := range []string{"first", "second"} {
		if err := os.WriteFile(file, []byte(want+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := token(); err != nil || got != want {
			t.Errorf("token = %q, %v, want %q", got, err, want)
		}
	}
	t.Setenv("FACTORY_API_TOKEN", "from-env")
	if got, _ := bearer("")(); got != "from-env" {
		t.Errorf("env token = %q", got)
	}
}
