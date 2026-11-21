package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const (
	healthy = `{"status":"ok"}`
	passing = `{"version":"dev","checks":[
{"name":"database reachable and migrated","status":"pass"},
{"name":"code host credential","status":"pass","detail":"the App authenticates and has 1 installation(s)"},
{"name":"sandbox runtime reachable","status":"skip","detail":"the launcher is off (--launcher=false), so the server starts no sandboxes"},
{"name":"cluster worker active","status":"pass","detail":"1 active"}]}`
	failing = `{"version":"v9.0.0","checks":[
{"name":"database reachable and migrated","status":"fail","detail":"database is behind this binary's migrations: run ` + "`factory migrate up`" + `","fix":"apply the migrations to the server's database with ` + "`factory migrate up`" + `","command":"factory migrate up"},
{"name":"code host credential","status":"fail","detail":"no GitHub App is configured","fix":"start the server with --github-app-id and --github-app-key-file"},
{"name":"sandbox runtime reachable","status":"pass","detail":"0 worker sandbox(es) exist"},
{"name":"cluster worker active","status":"fail","detail":"no cluster worker is active","fix":"start one"}]}`
)

func TestDoctor(t *testing.T) {
	cases := map[string]struct {
		routes map[string]reply
		args   []string
	}{
		"pass":         {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": ok(passing)}, nil},
		"pass_json":    {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": ok(passing)}, []string{"--output", "json"}},
		"fail":         {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": ok(failing)}, nil},
		"fail_json":    {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": ok(failing)}, []string{"--output", "json"}},
		"token":        {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": apiError(401, "unauthorized", "send the API token")}, nil},
		"older_server": {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": apiError(404, "not_found", "no route for GET /api/v1/admin/doctor")}, nil},
		"server_error": {map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": apiError(500, "internal", "internal error")}, nil},
		"not_factory":  {map[string]reply{"GET /healthz": {http.StatusNotFound, "404 page not found"}}, nil},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(apiTokenEnv, "")
			fakeServer(t, tt.routes)
			golden(t, "doctor/"+name, transcript(t.Context(), append([]string{"doctor"}, tt.args...)...))
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		golden(t, "doctor/unreachable", transcript(t.Context(), "doctor", "--server", "http://127.0.0.1:1"))
	})
}

// TestDoctorWorker runs the worker checks on a machine with nothing on PATH,
// so each fails the same way everywhere.
func TestDoctorWorker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	out := transcript(t.Context(), "doctor", "--worker", "--work-dir", filepath.Join(dir, "work"))
	golden(t, "doctor/worker", strings.ReplaceAll(out, dir, "$TMP"))
	golden(t, "doctor/worker_unknown_harness", transcript(t.Context(), "doctor", "--worker", "--harnesses", "gpt"))
}

// TestDoctorOffersTheFix answers the offer to run a failed check's command
// on a terminal, and checks a non-terminal is never asked.
func TestDoctorOffersTheFix(t *testing.T) {
	t.Setenv(apiTokenEnv, "")
	t.Setenv("FACTORY_DATABASE_URL", "")
	routes := map[string]reply{"GET /healthz": ok(healthy), "GET /api/v1/admin/doctor": ok(failing)}
	for _, tt := range []struct {
		answer  string
		wantRun bool
	}{{"\n", true}, {"y\n", true}, {"n\n", false}} {
		fakeServer(t, routes)
		var stdout, stderr bytes.Buffer
		code := dispatch(t.Context(), []string{"doctor"}, streams{in: strings.NewReader(tt.answer), out: &stdout, err: &stderr, interactive: true})
		ran := strings.Contains(stderr.String(), "factory migrate up: no database URL")
		if code != 1 || !strings.Contains(stdout.String(), "run 'factory migrate up' now? [Y/n]") || ran != tt.wantRun {
			t.Errorf("answer %q: exit %d, ran %v (want %v)\nstdout %s\nstderr %s", tt.answer, code, ran, tt.wantRun, stdout.String(), stderr.String())
		}
	}
	fakeServer(t, routes)
	if out := transcript(t.Context(), "doctor"); strings.Contains(out, "[Y/n]") {
		t.Errorf("a non-interactive run was asked:\n%s", out)
	}
}
