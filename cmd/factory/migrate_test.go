package main

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
)

func TestParseFlagsFallsBackToEnv(t *testing.T) {
	t.Setenv("FACTORY_DATABASE_URL", "from-env")
	t.Setenv("FACTORY_STUCK_AFTER", "from-env-too")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "env when the flag is absent", want: "from-env"},
		{name: "flag wins over env", args: []string{"--database-url", "from-flag"}, want: "from-flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("x", flag.ContinueOnError)
			got := fs.String("database-url", "", "")
			stuck := fs.String("stuck-after", "", "")
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			if *got != tt.want {
				t.Errorf("database-url = %q, want %q", *got, tt.want)
			}
			if *stuck != "from-env-too" {
				t.Errorf("stuck-after = %q, want the FACTORY_STUCK_AFTER value", *stuck)
			}
		})
	}
}

func TestMigrateUsage(t *testing.T) {
	t.Setenv(db.DatabaseURLEnv, "")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no subcommand", args: []string{"migrate"}, wantCode: 2, wantStderr: migrateUsage},
		{name: "help", args: []string{"migrate", "--help"}, wantStdout: migrateUsage},
		{
			name: "unknown subcommand", args: []string{"migrate", "sideways"}, wantCode: 2,
			wantStderr: "factory migrate: unknown subcommand \"sideways\"\n" + migrateUsage,
		},
		{
			name: "no database URL", args: []string{"migrate", "status"}, wantCode: 1,
			wantStderr: "factory migrate status: " + db.ErrNoDatabaseURL.Error() + "\n",
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

func TestMigrateAgainstPostgres(t *testing.T) {
	databaseURL := dbtest.URL(t)
	migrate := func(sub string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"migrate", sub, "--database-url", databaseURL}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, out, errOut := migrate("status")
	if code != 0 || !strings.Contains(out, "version 0") || !strings.Contains(out, "001_baseline  pending") {
		t.Fatalf("status before up: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut = migrate("up"); code != 0 {
		t.Fatalf("up: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, _ = migrate("status"); code != 0 || !strings.Contains(out, "001_baseline  applied") || !strings.Contains(out, "0 pending") {
		t.Fatalf("status after up: code %d, stdout %q", code, out)
	}
	if code, out, errOut = migrate("down"); code != 0 {
		t.Fatalf("down: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
