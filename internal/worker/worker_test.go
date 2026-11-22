package worker

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/harness"
)

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}

// fakeServer is a factory server that hands out one step, then nothing.
type fakeServer struct {
	mu       sync.Mutex
	held     []string
	claims   int
	calls    []string
	leaseSec string
}

func (s *fakeServer) handler(step string) http.Handler {
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	}
	mux.HandleFunc("POST /api/v1/workers", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = w.Write([]byte(`{"id":"w1"}`))
	})
	mux.HandleFunc("POST /api/v1/workers/w1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		held := `[]`
		if len(s.held) > 0 {
			held = `["` + s.held[0] + `"]`
		}
		lease := s.leaseSec
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"held":` + held + `,"lease_seconds":` + lease + `}`))
	})
	mux.HandleFunc("POST /api/v1/workers/w1/claim", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.claims++
		if s.claims > 1 || step == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(step))
	})
	for _, path := range []string{"POST /api/v1/steps/s1/result", "POST /api/v1/steps/s1/release", "POST /api/v1/steps/s1/stage"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			record(r)
			_, _ = w.Write([]byte(`{}`))
		})
	}
	return mux
}

func (s *fakeServer) called(call string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == call {
			return true
		}
	}
	return false
}

// blockingHost blocks in FindPull until its context ends.
type blockingHost struct {
	*codehost.Fake
	started chan struct{}
}

func (h blockingHost) FindPull(ctx context.Context, _, _ string) (codehost.Pull, bool, error) {
	close(h.started)
	<-ctx.Done()
	return codehost.Pull{}, false, ctx.Err()
}

// mergeStep is a claim on a merge step, which blockingHost holds up.
const mergeStep = `{"step":{"id":"s1","key":"o/r/merge","kind":"merge","params":{"repo":"o/r"}},
 "item":{"id":"i1","title":"t","repos":["o/r"]},"results":{},
 "outputs":[{"repo":"o/r","kind":"pull_request","branch":"b","url":"https://github.test/o/r/pull/1","status":"open"}]}`

func runConfig(t *testing.T, url string, host codehost.Host) Config {
	return Config{
		Client:    &Client{URL: url},
		Name:      "sandbox-1",
		WorkDir:   t.TempDir(),
		Harnesses: []harness.Harness{&harness.Fake{}},
		Host:      func(string, func(context.Context) (string, error)) codehost.Host { return host },
		Log:       testLogger(t),
	}
}

func TestRunExitsWhenNothingIsLeft(t *testing.T) {
	s := &fakeServer{leaseSec: "300"}
	if err := Run(t.Context(), runConfig(t, serve(t, s.handler("")), nil)); err != nil {
		t.Fatalf("Run = %v, want nil so the Job completes", err)
	}
	if !s.called("POST /api/v1/workers/w1/claim") {
		t.Error("Run did not claim")
	}
}

func TestRunReportsAndExits(t *testing.T) {
	s := &fakeServer{leaseSec: "300", held: []string{"s1"}}
	host := &codehost.Fake{Repo: "o/r", Pulls: []codehost.Pull{{Number: 1, State: "open"}}}
	if err := Run(t.Context(), runConfig(t, serve(t, s.handler(mergeStep)), host)); err != nil {
		t.Fatal(err)
	}
	if !s.called("POST /api/v1/steps/s1/result") || len(host.Merged()) != 1 {
		t.Errorf("calls = %v, merged %v", s.calls, host.Merged())
	}
}

func TestRunReleasesTheStepOnShutdown(t *testing.T) {
	s := &fakeServer{leaseSec: "300", held: []string{"s1"}}
	host := blockingHost{Fake: &codehost.Fake{}, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, runConfig(t, serve(t, s.handler(mergeStep)), host)) }()
	<-host.started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if !s.called("POST /api/v1/steps/s1/release") || s.called("POST /api/v1/steps/s1/result") {
		t.Errorf("calls = %v, want a release and no result", s.calls)
	}
}

func TestRunStopsAStepTheHeartbeatDrops(t *testing.T) {
	s := &fakeServer{leaseSec: "3"} // heartbeat every second; holds nothing
	host := blockingHost{Fake: &codehost.Fake{}, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), runConfig(t, serve(t, s.handler(mergeStep)), host)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lost step was not stopped")
	}
	if s.called("POST /api/v1/steps/s1/result") || s.called("POST /api/v1/steps/s1/release") {
		t.Errorf("calls = %v, want no report on a lost step", s.calls)
	}
}

// TestRunSwitchesToItsCredential sends the pod's token once, to register,
// and the credential the server answers with on every call after.
func TestRunSwitchesToItsCredential(t *testing.T) {
	var mu sync.Mutex
	auth := map[string][]string{}
	s := &fakeServer{leaseSec: "300", held: []string{"s1"}}
	inner := s.handler(mergeStep)
	url := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth[r.Header.Get("Authorization")] = append(auth[r.Header.Get("Authorization")], r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/v1/workers" {
			_, _ = w.Write([]byte(`{"id":"w1","credential":"fwc_secret"}`))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	cfg := runConfig(t, url, &codehost.Fake{Repo: "o/r", Pulls: []codehost.Pull{{Number: 1, State: "open"}}})
	cfg.Client.Token = func() (string, error) { return "pod-token", nil }
	if err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := auth["Bearer pod-token"]; len(got) != 1 || got[0] != "POST /api/v1/workers" {
		t.Errorf("pod token sent for %v, want the registration only", got)
	}
	if len(auth["Bearer fwc_secret"]) < 3 || len(auth) != 2 {
		t.Errorf("requests by token = %v, want everything after registering on the credential", auth)
	}
}
