package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
)

func TestServeRefusesBeforeListening(t *testing.T) {
	t.Setenv("FACTORY_API_TOKEN", "")
	t.Setenv(db.DatabaseURLEnv, "")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "help", args: []string{"--help"}},
		{name: "extra argument", args: []string{"now"}, wantCode: 2, wantStderr: "unexpected argument"},
		{name: "bad addr", args: []string{"--addr", "nope"}, wantCode: 2, wantStderr: "--addr"},
		{name: "public bind without a token", args: []string{"--addr", "0.0.0.0:8080"}, wantCode: 2, wantStderr: "Set FACTORY_API_TOKEN"},
		{name: "all interfaces without a token", args: []string{"--addr", ":8080"}, wantCode: 2, wantStderr: "Set FACTORY_API_TOKEN"},
		{name: "zero attempts", args: []string{"--max-attempts", "0"}, wantCode: 2, wantStderr: "max-attempts"},
		{name: "bad duration", args: []string{"--lease", "soon"}, wantCode: 2, wantStderr: "lease"},
		{name: "no database", args: nil, wantCode: 1, wantStderr: db.ErrNoDatabaseURL.Error()},
		{name: "app id without a key", args: []string{"--github-app-id", "12345"}, wantCode: 2, wantStderr: "--github-app-key-file"},
		{name: "key without an app id", args: []string{"--github-app-key-file", "app.pem"}, wantCode: 2, wantStderr: "--github-app-id"},
		{name: "missing key file", args: []string{"--github-app-id", "1", "--github-app-key-file", "/nonexistent/app.pem"}, wantCode: 1, wantStderr: "/nonexistent/app.pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), append([]string{"serve"}, tt.args...), &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// TestServeRefusesABadAppKeyWithoutEchoingIt names the key file, not its
// contents.
func TestServeRefusesABadAppKeyWithoutEchoingIt(t *testing.T) {
	t.Setenv("FACTORY_API_TOKEN", "")
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	const secret = "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n"
	if err := os.WriteFile(keyFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTORY_GITHUB_APP_ID", "12345")
	t.Setenv("FACTORY_GITHUB_APP_KEY_FILE", keyFile)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), keyFile) || strings.Contains(stderr.String(), "c2VjcmV0") {
		t.Fatalf("exit %d, stderr %q; want 1, the file name, and not the key", code, stderr.String())
	}
}

func TestServeRefusesAnUnmigratedDatabase(t *testing.T) {
	t.Setenv("FACTORY_API_TOKEN", "")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve", "--addr", "127.0.0.1:0", "--database-url", dbtest.URL(t)}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "run `factory migrate up`") {
		t.Fatalf("exit %d, stderr %q; want 1 and the fix", code, stderr.String())
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	t.Setenv("FACTORY_API_TOKEN", "")
	databaseURL := dbtest.URL(t)
	if err := db.Up(databaseURL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--database-url", databaseURL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"loopback only", "stopped"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("log %q does not mention %q", stderr.String(), want)
		}
	}
}
