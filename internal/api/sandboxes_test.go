package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/sandbox"
)

// newSandboxServer serves the API with a fake runtime and no database: the
// sandbox routes do not touch the work service.
func newSandboxServer(t *testing.T, rt sandbox.Runtime) *client {
	t.Helper()
	srv := httptest.NewServer(api.New(api.Config{Token: token, Logger: slog.New(slog.DiscardHandler), Sandboxes: rt}))
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

type logsResponse struct {
	Sandbox struct {
		Name  string `json:"name"`
		Key   string `json:"key"`
		Repo  string `json:"repo"`
		Phase string `json:"phase"`
	} `json:"sandbox"`
	Lines []struct {
		Time      time.Time `json:"time"`
		Text      string    `json:"text"`
		Truncated bool      `json:"truncated"`
	} `json:"lines"`
	Cursor  string `json:"cursor"`
	More    bool   `json:"more"`
	Pending *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"pending"`
}

func (r logsResponse) texts() []string {
	out := []string{}
	for _, l := range r.Lines {
		out = append(out, l.Text)
	}
	return out
}

func TestListSandboxes(t *testing.T) {
	t.Parallel()
	rt := sandbox.NewFake()
	ctx := t.Context()
	for _, sb := range []struct{ name, key string }{
		{"worker-b", "item-1/o/b"}, {"worker-a", "item-1/o/a"}, {"worker-x", "item-10/o/a"}, {"worker-y", ""},
	} {
		if _, err := rt.Ensure(ctx, sb.name, sb.key, ""); err != nil {
			t.Fatal(err)
		}
	}
	rt.SetPhase("worker-a", sandbox.PhaseRunning)
	rt.SetCreated("worker-b", time.Now().Add(-time.Minute))
	c := newSandboxServer(t, rt)

	var got struct {
		Sandboxes []struct {
			Name    string    `json:"name"`
			Key     string    `json:"key"`
			Repo    string    `json:"repo"`
			Phase   string    `json:"phase"`
			Created time.Time `json:"created"`
		} `json:"sandboxes"`
	}
	if resp := c.do(http.MethodGet, "/api/v1/work/item-1/sandboxes", "", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("sandboxes = %+v, want item-1's two, not item-10's", got.Sandboxes)
	}
	a := got.Sandboxes[0]
	if a.Name != "worker-b" || got.Sandboxes[1].Name != "worker-a" {
		t.Errorf("order = %s, %s; want oldest first", a.Name, got.Sandboxes[1].Name)
	}
	if b := got.Sandboxes[1]; b.Key != "item-1/o/a" || b.Repo != "o/a" || b.Phase != "running" || b.Created.IsZero() {
		t.Errorf("sandbox = %+v", b)
	}

	var empty map[string][]any
	if resp := c.do(http.MethodGet, "/api/v1/work/none/sandboxes", "", &empty); resp.StatusCode != http.StatusOK || empty["sandboxes"] == nil || len(empty["sandboxes"]) != 0 {
		t.Errorf("unknown item: %d %v, want 200 and an empty list", resp.StatusCode, empty)
	}
}

func TestSandboxLogs(t *testing.T) {
	t.Parallel()
	rt := sandbox.NewFake()
	if _, err := rt.Ensure(t.Context(), "worker-a", "item-1/o/a", ""); err != nil {
		t.Fatal(err)
	}
	c := newSandboxServer(t, rt)
	path := "/api/v1/work/item-1/sandboxes/worker-a/logs"

	var pending logsResponse
	if resp := c.do(http.MethodGet, path, "", &pending); resp.StatusCode != http.StatusOK {
		t.Fatalf("pending: status %d", resp.StatusCode)
	}
	if pending.Pending == nil || pending.Pending.Reason != "ContainerCreating" || pending.Cursor != "" || pending.Lines == nil || pending.Sandbox.Phase != "pending" {
		t.Errorf("pending = %+v, want its waiting reason, no lines and no cursor", pending)
	}

	rt.SetPhase("worker-a", sandbox.PhaseRunning)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i, text := range []string{"one", "two", "three"} {
		rt.AppendLog("worker-a", t0.Add(time.Duration(i)*time.Millisecond), text)
	}
	var tail logsResponse
	if resp := c.do(http.MethodGet, path+"?tail=2", "", &tail); resp.StatusCode != http.StatusOK {
		t.Fatalf("tail: status %d", resp.StatusCode)
	}
	if got := tail.texts(); len(got) != 2 || got[0] != "two" || got[1] != "three" || tail.Pending != nil || tail.More || tail.Cursor == "" {
		t.Errorf("tail = %+v, want two, three and a cursor", tail)
	}
	if tail.Sandbox.Name != "worker-a" || tail.Sandbox.Repo != "o/a" || tail.Sandbox.Phase != "running" || !tail.Lines[0].Time.Equal(t0.Add(time.Millisecond)) {
		t.Errorf("tail = %+v", tail)
	}

	rt.AppendLog("worker-a", t0.Add(3*time.Millisecond), "four")
	var next logsResponse
	c.do(http.MethodGet, path+"?after="+url.QueryEscape(tail.Cursor), "", &next)
	if got := next.texts(); len(got) != 1 || got[0] != "four" {
		t.Errorf("after the tail = %v, want only four", got)
	}
	var none logsResponse
	c.do(http.MethodGet, path+"?after="+url.QueryEscape(next.Cursor), "", &none)
	if len(none.Lines) != 0 || none.Cursor != next.Cursor {
		t.Errorf("with nothing new = %+v, want no lines and the same cursor", none)
	}
}

func TestSandboxLogsErrors(t *testing.T) {
	t.Parallel()
	rt := sandbox.NewFake()
	if _, err := rt.Ensure(t.Context(), "worker-a", "item-1/o/a", ""); err != nil {
		t.Fatal(err)
	}
	rt.SetPhase("worker-a", sandbox.PhaseRunning)
	if _, err := rt.Ensure(t.Context(), "worker-b", "item-1/o/b", ""); err != nil {
		t.Fatal(err)
	}
	rt.SetPhase("worker-b", sandbox.PhaseSucceeded)
	c := newSandboxServer(t, rt)
	tests := []struct {
		name, path string
		status     int
		code       string
	}{
		{"gone", "/api/v1/work/item-1/sandboxes/worker-gone/logs", http.StatusNotFound, "sandbox_not_found"},
		{"another item's", "/api/v1/work/item-2/sandboxes/worker-a/logs", http.StatusNotFound, "sandbox_not_found"},
		{"tail not a number", "/api/v1/work/item-1/sandboxes/worker-a/logs?tail=x", http.StatusUnprocessableEntity, "invalid_request"},
		{"tail zero", "/api/v1/work/item-1/sandboxes/worker-a/logs?tail=0", http.StatusUnprocessableEntity, "invalid_request"},
		{"tail too large", "/api/v1/work/item-1/sandboxes/worker-a/logs?tail=5001", http.StatusUnprocessableEntity, "invalid_request"},
		{"bad cursor", "/api/v1/work/item-1/sandboxes/worker-a/logs?after=yesterday", http.StatusUnprocessableEntity, "invalid_request"},
		{"tail and cursor", "/api/v1/work/item-1/sandboxes/worker-a/logs?tail=5&after=0-0", http.StatusUnprocessableEntity, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var e apiError
			if resp := c.do(http.MethodGet, tt.path, "", &e); resp.StatusCode != tt.status || e.Error.Code != tt.code || e.Error.Message == "" {
				t.Errorf("%d %+v, want %d %s with a message", resp.StatusCode, e.Error, tt.status, tt.code)
			}
		})
	}
	t.Run("destroyed", func(t *testing.T) {
		t.Parallel()
		var before logsResponse
		if resp := c.do(http.MethodGet, "/api/v1/work/item-1/sandboxes/worker-b/logs", "", &before); resp.StatusCode != http.StatusOK || before.Sandbox.Phase != "succeeded" {
			t.Fatalf("ended sandbox before removal: %d %+v, want 200 with its log", resp.StatusCode, before)
		}
		if err := rt.Destroy(t.Context(), "worker-b"); err != nil {
			t.Fatal(err)
		}
		var e apiError
		resp := c.do(http.MethodGet, "/api/v1/work/item-1/sandboxes/worker-b/logs", "", &e)
		if resp.StatusCode != http.StatusNotFound || e.Error.Code != "sandbox_not_found" {
			t.Errorf("%d %+v, want 404 sandbox_not_found", resp.StatusCode, e.Error)
		}
	})
}

func TestSandboxRoutesWithoutLauncher(t *testing.T) {
	t.Parallel()
	c := newSandboxServer(t, nil)
	for _, path := range []string{"/api/v1/work/i/sandboxes", "/api/v1/work/i/sandboxes/w/logs"} {
		var e apiError
		if resp := c.do(http.MethodGet, path, "", &e); resp.StatusCode != http.StatusServiceUnavailable || e.Error.Code != "sandboxes_not_configured" {
			t.Errorf("%s: %d %+v, want 503 sandboxes_not_configured", path, resp.StatusCode, e.Error)
		}
	}
	// The token still guards the routes.
	resp, err := http.Get(c.srv.URL + "/api/v1/work/i/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without the token: %d, want 401", resp.StatusCode)
	}
}
