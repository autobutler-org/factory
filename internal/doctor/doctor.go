// Package doctor holds the checks `factory doctor` reports: each answers
// pass, fail or skip, and a failure carries the fix. The server runs the
// database, code host, sandbox runtime and worker checks for
// GET /api/v1/admin/doctor; a worker image runs the git, agent CLI and
// workspace checks; the CLI compares versions. The package only checks; it
// changes nothing, prints nothing, and knows no HTTP.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/harness"
)

// Statuses of a check.
const (
	Pass = "pass"
	Fail = "fail"
	// Skip is a check that does not apply here, such as the sandbox runtime
	// with the launcher off. It does not fail doctor.
	Skip = "skip"
)

// checkTimeout bounds one check.
const checkTimeout = 15 * time.Second

// Result is one check's answer.
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Fix says what to do about a failure.
	Fix string `json:"fix,omitempty"`
	// Command is a factory command line that fixes the failure, such as
	// "factory migrate up", which the CLI may offer to run.
	Command string `json:"command,omitempty"`
}

// Failed names the checks that failed, in order.
func Failed(results []Result) []string {
	var names []string
	for _, r := range results {
		if r.Status == Fail {
			names = append(names, r.Name)
		}
	}
	return names
}

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, checkTimeout)
}

// Database checks that the server's database answers and is migrated;
// ready is the server's readiness check.
func Database(ctx context.Context, ready func(context.Context) error) Result {
	ctx, cancel := bounded(ctx)
	defer cancel()
	r := Result{Name: "database reachable and migrated"}
	err := ready(ctx)
	switch {
	case err == nil:
		r.Status = Pass
	case errors.Is(err, db.ErrMigrationsPending):
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "apply the migrations to the server's database with `factory migrate up`"
		r.Command = "factory migrate up"
	default:
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "check that Postgres is up and that the server's --database-url points at it"
	}
	return r
}

// CodeHost checks that the GitHub App credential is accepted and the App is
// installed somewhere; installations is nil when the server has no App.
func CodeHost(ctx context.Context, installations func(context.Context) (int, error)) Result {
	r := Result{Name: "code host credential"}
	if installations == nil {
		r.Status, r.Detail = Fail, "no GitHub App is configured, so workers get no repository tokens and repositories' .factory/config.toml is not read"
		r.Fix = "start the server with --github-app-id and --github-app-key-file"
		return r
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	n, err := installations(ctx)
	switch {
	case err != nil:
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "check --github-app-id and the key in --github-app-key-file against the App's settings, and that the server reaches --github-api-url"
	case n == 0:
		r.Status, r.Detail = Fail, "the GitHub App is installed nowhere, so it can mint no repository token"
		r.Fix = "install the App on the repositories the factory works on"
	default:
		r.Status, r.Detail = Pass, fmt.Sprintf("the App authenticates and has %d installation(s)", n)
	}
	return r
}

// Sandbox checks that the sandbox runtime (Kubernetes) answers; sandboxes
// lists the worker sandboxes, and is nil when the launcher is off.
func Sandbox(ctx context.Context, sandboxes func(context.Context) (int, error)) Result {
	r := Result{Name: "sandbox runtime reachable"}
	if sandboxes == nil {
		r.Status, r.Detail = Skip, "the launcher is off (--launcher=false), so the server starts no sandboxes"
		return r
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	n, err := sandboxes(ctx)
	if err != nil {
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "check that the server's ServiceAccount may list Jobs in --launcher-namespace and that the namespace exists"
		return r
	}
	r.Status, r.Detail = Pass, fmt.Sprintf("%d worker sandbox(es) exist", n)
	return r
}

// Workers checks that at least one cluster worker is active; active counts
// them.
func Workers(ctx context.Context, active func(context.Context) (int, error)) Result {
	ctx, cancel := bounded(ctx)
	defer cancel()
	r := Result{Name: "cluster worker active"}
	n, err := active(ctx)
	switch {
	case err != nil:
		r.Status, r.Detail, r.Fix = Fail, err.Error(), "check the database with the database check above"
	case n == 0:
		r.Status, r.Detail = Fail, "no cluster worker is active"
		r.Fix = "with --launcher on, a worker starts when an item is ready (enqueue one with `factory work enqueue`); otherwise start `factory worker` in a sandbox"
	default:
		r.Status, r.Detail = Pass, fmt.Sprintf("%d active", n)
	}
	return r
}

// release matches the vMAJOR.MINOR of a release build's version.
var release = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)

// Version checks that this binary and the server can work together: the same
// release, or releases with the same major and minor version. A build that is
// not a release, such as dev, cannot be compared and is skipped.
func Version(client, server string) Result {
	r := Result{Name: "version compatible", Detail: fmt.Sprintf("this factory %s, server %s", client, server)}
	c, s := release.FindStringSubmatch(client), release.FindStringSubmatch(server)
	switch {
	case client == server:
		r.Status = Pass
	case c == nil || s == nil:
		r.Status = Skip
		r.Detail += "; a build that is not a release cannot be compared"
	case c[1] == s[1] && c[2] == s[2]:
		r.Status = Pass
	default:
		r.Status = Fail
		r.Fix = fmt.Sprintf("install factory %s to match the server, or upgrade the server", server)
	}
	return r
}

// Git checks that git runs.
func Git(ctx context.Context) Result {
	ctx, cancel := bounded(ctx)
	defer cancel()
	r := Result{Name: "git"}
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "install git in the worker image and put it on PATH"
		return r
	}
	r.Status, r.Detail = Pass, strings.TrimSpace(string(out))
	return r
}

// Harnesses checks each agent CLI: installed and logged in, which is where
// its model credential lives. The worker skips a harness that is not ready,
// so one not ready is a skip while another passes, and a failure only when
// none is ready.
func Harnesses(ctx context.Context, hs []harness.Harness) []Result {
	ctx, cancel := bounded(ctx)
	defer cancel()
	results := make([]Result, len(hs))
	anyReady := false
	for i, h := range hs {
		results[i] = Result{Name: "agent CLI " + h.Name(), Status: Pass, Detail: "installed and logged in"}
		if err := h.Check(ctx); err != nil {
			results[i].Status, results[i].Detail = Skip, err.Error()
			results[i].Fix = "install it and log in, or leave it out of --harnesses"
			continue
		}
		anyReady = true
	}
	if !anyReady {
		for i := range results {
			results[i].Status = Fail
		}
	}
	return results
}

// Workspace checks that dir, where the worker keeps checkouts, can be
// created and written.
func Workspace(dir string) Result {
	r := Result{Name: "workspace writable", Detail: dir}
	fail := func(err error) Result {
		r.Status, r.Detail = Fail, err.Error()
		r.Fix = "make " + dir + " writable by the worker's user, or point --work-dir at a directory that is"
		return r
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fail(err)
	}
	_ = f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return fail(err)
	}
	r.Status = Pass
	return r
}
