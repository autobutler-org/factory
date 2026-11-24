package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testNetWait keeps a test's network wait short.
var testNetWait = netWait{first: time.Millisecond, most: 5 * time.Millisecond, total: 200 * time.Millisecond}

// noDNS fails the first n requests as a pod without DNS does, then sends
// the rest on.
type noDNS struct {
	mu       sync.Mutex
	n, tries int
}

func (d *noDNS) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.tries++
	fail := d.tries <= d.n
	d.mu.Unlock()
	if fail {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: r.URL.Hostname(), IsNotFound: true}}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestRunWaitsForTheNetwork(t *testing.T) {
	s := &fakeServer{leaseSec: "300"}
	cfg := runConfig(t, serve(t, s.handler("")), nil)
	dns := &noDNS{n: 3}
	cfg.Client.HTTP = &http.Client{Transport: dns}
	cfg.netWait = testNetWait
	if err := Run(t.Context(), cfg); err != nil {
		t.Fatalf("Run = %v, want it to wait for the network and go on", err)
	}
	if dns.tries <= 3 || !s.called("POST /api/v1/workers") || !s.called("POST /api/v1/workers/w1/claim") {
		t.Errorf("tries = %d, calls = %v; want registration after 3 failed lookups, then a claim", dns.tries, s.calls)
	}
}

func TestRunSaysTheNetworkWasNotReady(t *testing.T) {
	s := &fakeServer{leaseSec: "300"}
	cfg := runConfig(t, serve(t, s.handler("")), nil)
	cfg.Client.HTTP = &http.Client{Transport: &noDNS{n: 1 << 30}}
	cfg.netWait = testNetWait
	err := Run(t.Context(), cfg)
	if !errors.Is(err, ErrNetworkNotReady) || !strings.Contains(err.Error(), "no such host") || !strings.Contains(err.Error(), "docs/operations.md") {
		t.Fatalf("Run = %v, want ErrNetworkNotReady with the lookup error and where to look", err)
	}
	if s.called("POST /api/v1/workers") {
		t.Error("registered without a network")
	}
}

func TestNetWaitDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	refused := errors.New("401 unauthorized")
	err := testNetWait.do(t.Context(), testLogger(t), "call", unreachable, func() error {
		calls++
		return refused
	})
	if !errors.Is(err, refused) || calls != 1 {
		t.Errorf("err = %v after %d calls, want the error after one", err, calls)
	}
}

func TestNetWaitStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err := netWait{first: time.Hour, most: time.Hour, total: 2 * time.Hour}.do(ctx, nil, "call", func(error) bool { return true }, func() error {
		calls++
		cancel()
		return errors.New("no such host")
	})
	if err == nil || calls != 1 {
		t.Errorf("err = %v after %d calls, want the error once the context ends", err, calls)
	}
}

func TestGitUnreachable(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com", true},
		{"fatal: unable to access 'https://github.com/o/r.git/': Failed to connect to github.com port 443 after 3 ms: Couldn't connect to server", true},
		{"fatal: unable to access 'https://github.com/o/r.git/': Connection timed out after 10001 milliseconds", true},
		{"remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found", false},
		{"fatal: Authentication failed for 'https://github.com/o/r.git/'", false},
		{"! [rejected] main -> main (fetch first)", false},
	} {
		if got := gitUnreachable(fmt.Errorf("git ls-remote: exit status 128: %s", tc.out)); got != tc.want {
			t.Errorf("gitUnreachable(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// flakyGit puts a git in front of the real one whose first n commands that
// talk to the remote fail as git does without DNS.
func flakyGit(t *testing.T, real string, n int) string {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
  ls-remote|fetch|push)
    n=$(cat '` + count + `' 2>/dev/null || echo 0)
    if [ "$n" -lt ` + strconv.Itoa(n) + ` ]; then
      echo $((n + 1)) > '` + count + `'
      echo "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com" >&2
      exit 128
    fi
    break ;;
  esac
done
exec '` + real + `' "$@"
`
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestWorkspaceWaitsForTheNetwork(t *testing.T) {
	ctx := t.Context()
	t.Run("the first lookups fail, then the clone goes on", func(t *testing.T) {
		f := newFixture(t, "true")
		f.e.cfg.netWait = testNetWait
		f.e.git = flakyGit(t, f.e.git, 3)
		mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
	})
	t.Run("a network that never comes is retryable", func(t *testing.T) {
		f := newFixture(t, "true")
		f.e.cfg.netWait = testNetWait
		f.e.git = flakyGit(t, f.e.git, 1<<30)
		r := f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)).report()
		if r.Outcome != "failed" || !r.Retryable || !strings.Contains(r.Reason, "the network was not ready") || !strings.Contains(r.Reason, "Could not resolve host") {
			t.Errorf("workspace = %+v, want a retryable failure that says the network was not ready", r)
		}
	})
}
