package worker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder is a fake factory server that answers each path with a canned
// reply and records the requests.
type recorder struct {
	t       *testing.T
	replies map[string]func(w http.ResponseWriter)
	bodies  map[string]string
	auth    []string
}

func newRecorder(t *testing.T, replies map[string]func(w http.ResponseWriter)) (*recorder, *Client) {
	r := &recorder{t: t, replies: replies, bodies: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.bodies[req.Method+" "+req.URL.Path] = string(body)
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		reply, ok := r.replies[req.Method+" "+req.URL.Path]
		if !ok {
			t.Errorf("unexpected %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(w)
	}))
	t.Cleanup(srv.Close)
	return r, &Client{URL: srv.URL, Token: func() (string, error) { return "secret", nil }}
}

func reply(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestClientProtocol(t *testing.T) {
	rec, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"POST /api/v1/workers":               reply(200, `{"id":"w1","name":"n"}`),
		"POST /api/v1/workers/w1/heartbeat":  reply(200, `{"held":["s1"],"lease_seconds":30}`),
		"POST /api/v1/workers/w1/claim":      reply(200, `{"step":{"id":"s1","key":"o/r/gate","kind":"gate","params":{"repo":"o/r"},"sandbox_key":"i/o/r","attempts":1},"item":{"id":"i","source":null,"external_url":null,"title":"t","repos":["o/r"]},"results":{"o/r/workspace":{"branch":"b"}},"outputs":[{"id":3,"repo":"o/r","kind":"pull_request","url":"u","status":"open"}]}`),
		"POST /api/v1/steps/s1/stage":        reply(200, `{}`),
		"POST /api/v1/steps/s1/result":       reply(200, `{}`),
		"POST /api/v1/steps/s1/release":      reply(200, `{}`),
		"POST /api/v1/work/i/codehost-token": reply(200, `{"repo":"o/r","token":"ghs_x","expires_at":"2026-11-18T13:00:00Z"}`),
		"POST /api/v1/workers/empty/claim":   reply(204, ``),
	})
	ctx := t.Context()
	id, err := c.Register(ctx, "n", "v1", []string{"claude-code"})
	if err != nil || id != "w1" {
		t.Fatalf("Register = %q, %v", id, err)
	}
	if got := rec.bodies["POST /api/v1/workers"]; !strings.Contains(got, `"kind":"cluster"`) || !strings.Contains(got, `"harnesses":["claude-code"]`) {
		t.Errorf("register body = %s", got)
	}
	held, lease, err := c.Heartbeat(ctx, "w1")
	if err != nil || len(held) != 1 || held[0] != "s1" || lease != 30*time.Second {
		t.Errorf("Heartbeat = %v, %s, %v", held, lease, err)
	}
	claim, ok, err := c.Claim(ctx, "w1", "i/o/r")
	if err != nil || !ok || claim.Step.Kind != "gate" || claim.Item.ExternalURL != "" || claim.Outputs[0].URL != "u" || string(claim.Results["o/r/workspace"]) != `{"branch":"b"}` {
		t.Errorf("Claim = %+v, %v, %v", claim, ok, err)
	}
	if got := rec.bodies["POST /api/v1/workers/w1/claim"]; got != `{"sandbox_key":"i/o/r"}` {
		t.Errorf("claim body = %s", got)
	}
	if _, ok, err := c.Claim(ctx, "empty", ""); ok || err != nil {
		t.Errorf("Claim on 204 = %v, %v, want not ok and no error", ok, err)
	}
	if got := rec.bodies["POST /api/v1/workers/empty/claim"]; got != "" {
		t.Errorf("claim without a sandbox key sent %q, want no body", got)
	}
	if err := c.Stage(ctx, "s1", "w1", "gate_passed", nil); err != nil {
		t.Error(err)
	}
	r := Result{Outcome: "succeeded", Result: map[string]string{"url": "u"}, Outputs: []Output{{Repo: "o/r", Kind: "pull_request", URL: "u", Status: "open"}}}
	if err := c.Report(ctx, "s1", "w1", r); err != nil {
		t.Error(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.bodies["POST /api/v1/steps/s1/result"]), &sent); err != nil || sent["worker_id"] != "w1" || sent["outcome"] != "succeeded" {
		t.Errorf("result body = %s", rec.bodies["POST /api/v1/steps/s1/result"])
	}
	if err := c.Release(ctx, "s1", "w1", "shutting down"); err != nil {
		t.Error(err)
	}
	token, err := c.CodehostToken(ctx, "i", "w1", "o/r")
	if err != nil || token.Token != "ghs_x" || token.ExpiresAt.IsZero() {
		t.Errorf("CodehostToken = %+v, %v", token, err)
	}
	for _, a := range rec.auth {
		if a != "Bearer secret" {
			t.Errorf("Authorization = %q, want the bearer token on every request", a)
		}
	}
}

func TestClientErrors(t *testing.T) {
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"POST /api/v1/steps/lost/result":     reply(409, `{"error":{"code":"lease_expired","message":"the lease ran out"}}`),
		"POST /api/v1/steps/other/result":    reply(409, `{"error":{"code":"not_claimant","message":"no"}}`),
		"POST /api/v1/steps/bad/result":      reply(422, `{"error":{"code":"invalid_request","message":"bad outcome"}}`),
		"POST /api/v1/work/i/codehost-token": reply(503, `{"error":{"code":"codehost_not_configured","message":"no app"}}`),
	})
	ctx := t.Context()
	for _, id := range []string{"lost", "other"} {
		if err := c.Report(ctx, id, "w", Result{Outcome: "succeeded"}); !errors.Is(err, ErrLost) {
			t.Errorf("Report %s = %v, want ErrLost", id, err)
		}
	}
	err := c.Report(ctx, "bad", "w", Result{Outcome: "x"})
	var apiErr *APIError
	if errors.Is(err, ErrLost) || !errors.As(err, &apiErr) || apiErr.Status != 422 || apiErr.Code != "invalid_request" {
		t.Errorf("Report bad = %v, want a 422 APIError", err)
	}
	if _, err := c.CodehostToken(ctx, "i", "w", "o/r"); err == nil || !strings.Contains(err.Error(), "codehost_not_configured") {
		t.Errorf("CodehostToken = %v, want the server's code", err)
	}
	unreachable := &Client{URL: "http://127.0.0.1:1"}
	if _, err := unreachable.Register(ctx, "n", "v", nil); err == nil || !strings.Contains(err.Error(), "reachable") {
		t.Errorf("Register on a dead server = %v, want advice to check the server", err)
	}
}
