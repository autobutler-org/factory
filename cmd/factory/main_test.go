package main

import (
	"bytes"
	"context"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args prints usage to stderr", wantCode: 2, wantStderr: usage},
		{name: "short help", args: []string{"-h"}, wantStdout: usage},
		{name: "long help", args: []string{"--help"}, wantStdout: usage},
		{
			name: "unknown command", args: []string{"nope"}, wantCode: 2,
			wantStderr: "factory: unknown command \"nope\"\nRun 'factory --help' for usage.\n",
		},
		{
			name: "unknown flag", args: []string{"--nope"}, wantCode: 2,
			wantStderr: "factory: unknown command \"--nope\"\nRun 'factory --help' for usage.\n",
		},
		{name: "version unstamped", args: []string{"version"}, wantStdout: "factory dev (commit none, built unknown)\n"},
		{name: "version help", args: []string{"version", "--help"}, wantStdout: versionUsage},
		{
			name: "version extra argument", args: []string{"version", "extra"}, wantCode: 2,
			wantStderr: "factory version: takes no arguments\n" + versionUsage,
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
