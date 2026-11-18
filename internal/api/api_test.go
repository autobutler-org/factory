package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/work"
)

const token = "s3cret"

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newServer(t *testing.T, ready func(context.Context) error) *client {
	t.Helper()
	pool := dbtest.NewDB(t)
	if ready == nil {
		ready = func(context.Context) error { return nil }
	}
	h := api.New(api.Config{
		Work:   work.New(pool),
		Ready:  ready,
		Token:  token,
		Logger: slog.New(slog.DiscardHandler),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// do sends a request with the bearer token and decodes a JSON response into out.
func (c *client) do(method, path, body string, out any, headers ...string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp
}

type item struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	State       string   `json:"state"`
	Priority    int      `json:"priority"`
	Repos       []string `json:"repos"`
	StateReason string   `json:"state_reason"`
}

type enqueued struct {
	Item    item   `json:"item"`
	Created bool   `json:"created"`
	Changed bool   `json:"changed"`
	Reason  string `json:"reason"`
}

type apiError struct {
	Error struct {
		Code         string   `json:"code"`
		Message      string   `json:"message"`
		RequestID    string   `json:"request_id"`
		CurrentState string   `json:"current_state"`
		Allowed      []string `json:"allowed"`
	} `json:"error"`
}

func (c *client) enqueue(body string) enqueued {
	c.t.Helper()
	var out enqueued
	if resp := c.do(http.MethodPost, "/api/v1/work", body, &out); resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.t.Fatalf("enqueue %s: %d", body, resp.StatusCode)
	}
	return out
}

func TestHealthAndReadiness(t *testing.T) {
	c := newServer(t, func(context.Context) error { return errors.New("database is behind: run `factory migrate up`") })
	if resp := c.do(http.MethodGet, "/healthz", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
	var e apiError
	resp := c.do(http.MethodGet, "/readyz", "", &e)
	if resp.StatusCode != http.StatusServiceUnavailable || e.Error.Code != "not_ready" || !strings.Contains(e.Error.Message, "factory migrate up") {
		t.Errorf("/readyz = %d %+v, want 503 not_ready naming the fix", resp.StatusCode, e)
	}
	ok := newServer(t, nil)
	if resp := ok.do(http.MethodGet, "/readyz", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz on a ready server = %d, want 200", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	c := newServer(t, nil)
	for _, header := range []string{"", "Bearer wrong", "Basic " + token, token} {
		req, _ := http.NewRequest(http.MethodGet, c.srv.URL+"/api/v1/work", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e apiError
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || e.Error.Code != "unauthorized" {
			t.Errorf("Authorization %q: %d %q, want 401 unauthorized", header, resp.StatusCode, e.Error.Code)
		}
	}
	// Health checks need no token.
	resp, err := http.Get(c.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz without a token = %d, want 200", resp.StatusCode)
	}
}

func TestNoTokenMeansNoAuth(t *testing.T) {
	pool := dbtest.NewDB(t)
	srv := httptest.NewServer(api.New(api.Config{Work: work.New(pool), Ready: func(context.Context) error { return nil }, Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/work")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no token configured: %d, want 200 (the server binds loopback instead)", resp.StatusCode)
	}
}

func TestRequestIDAndUnknownRoute(t *testing.T) {
	c := newServer(t, nil)
	var e apiError
	resp := c.do(http.MethodGet, "/api/v1/nope", "", &e, "X-Request-Id", "abc-123")
	if resp.StatusCode != http.StatusNotFound || e.Error.Code != "not_found" {
		t.Errorf("unknown route: %d %q, want 404 not_found", resp.StatusCode, e.Error.Code)
	}
	if got := resp.Header.Get("X-Request-ID"); got != "abc-123" || e.Error.RequestID != "abc-123" {
		t.Errorf("request id header %q, body %q; want the caller's abc-123", got, e.Error.RequestID)
	}
	resp = c.do(http.MethodGet, "/healthz", "", nil)
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("no request id generated")
	}
}

func TestEnqueueByValue(t *testing.T) {
	c := newServer(t, nil)
	body := `{"source":"github","external_id":"o/r#1","external_url":"https://github.com/o/r/issues/1","title":"t","repos":["o/r"],"priority":2}`
	var first enqueued
	resp := c.do(http.MethodPost, "/api/v1/work", body, &first)
	if resp.StatusCode != http.StatusCreated || !first.Created || first.Item.State != work.Ready || first.Item.Priority != 2 {
		t.Fatalf("first enqueue: %d %+v, want 201 created ready", resp.StatusCode, first)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/work/"+first.Item.ID {
		t.Errorf("Location = %q", loc)
	}
	var again enqueued
	resp = c.do(http.MethodPost, "/api/v1/work", strings.Replace(body, `"title":"t"`, `"title":"t2"`, 1), &again)
	if resp.StatusCode != http.StatusOK || again.Created || again.Item.ID != first.Item.ID || again.Item.Title != "t2" {
		t.Fatalf("repeat enqueue: %d %+v, want 200 with the same item retitled", resp.StatusCode, again)
	}

	// Once work has started, enqueueing again changes nothing and says why.
	if resp := c.do(http.MethodPost, "/api/v1/work/"+first.Item.ID+"/transitions", `{"to":"in_progress"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("transition: %d", resp.StatusCode)
	}
	var frozen enqueued
	resp = c.do(http.MethodPost, "/api/v1/work", body, &frozen)
	if resp.StatusCode != http.StatusOK || frozen.Changed || frozen.Reason == "" || frozen.Item.State != work.InProgress {
		t.Fatalf("enqueue of started work: %d %+v, want 200 unchanged with a reason", resp.StatusCode, frozen)
	}
}

func TestEnqueueIdempotencyKey(t *testing.T) {
	c := newServer(t, nil)
	var first, second enqueued
	if resp := c.do(http.MethodPost, "/api/v1/work", `{"title":"nightly"}`, &first, "Idempotency-Key", "k1"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	if resp := c.do(http.MethodPost, "/api/v1/work", `{"title":"nightly"}`, &second, "Idempotency-Key", "k1"); resp.StatusCode != http.StatusOK || second.Item.ID != first.Item.ID {
		t.Fatalf("retry: %d %+v, want 200 and the first item", resp.StatusCode, second)
	}
	var list struct {
		Items []item `json:"items"`
	}
	c.do(http.MethodGet, "/api/v1/work", "", &list)
	if len(list.Items) != 1 {
		t.Fatalf("%d items, want 1", len(list.Items))
	}
}

func TestEnqueueValidation(t *testing.T) {
	c := newServer(t, nil)
	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{name: "malformed", body: `{`, status: http.StatusBadRequest, code: "malformed_request"},
		{name: "unknown field", body: `{"title":"t","colour":"red"}`, status: http.StatusBadRequest, code: "malformed_request"},
		{name: "no title", body: `{}`, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "bad state", body: `{"title":"t","state":"completed"}`, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "unknown dependency", body: `{"title":"t","blocked_by":["github:x#1"]}`, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "too large", body: `{"title":"` + strings.Repeat("x", 2<<20) + `"}`, status: http.StatusRequestEntityTooLarge, code: "body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e apiError
			resp := c.do(http.MethodPost, "/api/v1/work", tt.body, &e)
			if resp.StatusCode != tt.status || e.Error.Code != tt.code {
				t.Errorf("%d %q (%s), want %d %q", resp.StatusCode, e.Error.Code, e.Error.Message, tt.status, tt.code)
			}
		})
	}
}

func TestWorkRoutes(t *testing.T) {
	c := newServer(t, nil)
	a := c.enqueue(`{"title":"a","repos":["o/a"],"priority":1}`).Item
	b := c.enqueue(`{"title":"b","repos":["o/b"],"state":"design"}`).Item

	var got item
	if resp := c.do(http.MethodGet, "/api/v1/work/"+a.ID, "", &got); resp.StatusCode != http.StatusOK || got.ID != a.ID {
		t.Fatalf("get: %d %+v", resp.StatusCode, got)
	}
	var e apiError
	if resp := c.do(http.MethodGet, "/api/v1/work/00000000-0000-0000-0000-000000000000", "", &e); resp.StatusCode != http.StatusNotFound || e.Error.Code != "not_found" {
		t.Errorf("get missing: %d %q", resp.StatusCode, e.Error.Code)
	}

	if resp := c.do(http.MethodPatch, "/api/v1/work/"+a.ID, `{"title":"a2","priority":9}`, &got); resp.StatusCode != http.StatusOK || got.Title != "a2" || got.Priority != 9 {
		t.Errorf("patch: %d %+v", resp.StatusCode, got)
	}

	var list struct {
		Items      []item `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	c.do(http.MethodGet, "/api/v1/work?state=ready&repo=o/a", "", &list)
	if len(list.Items) != 1 || list.Items[0].ID != a.ID {
		t.Errorf("filtered list = %+v", list.Items)
	}
	c.do(http.MethodGet, "/api/v1/work?limit=1", "", &list)
	if len(list.Items) != 1 || list.NextCursor == "" {
		t.Fatalf("first page = %+v", list)
	}
	c.do(http.MethodGet, "/api/v1/work?limit=1&cursor="+list.NextCursor, "", &list)
	if len(list.Items) != 1 || list.Items[0].ID != b.ID || list.NextCursor != "" {
		t.Errorf("second page = %+v, want b and no cursor", list)
	}
	if resp := c.do(http.MethodGet, "/api/v1/work?limit=x", "", &e); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad limit: %d", resp.StatusCode)
	}

	// Dependencies.
	if resp := c.do(http.MethodPut, "/api/v1/work/"+a.ID+"/dependencies/"+b.ID, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("put dependency: %d", resp.StatusCode)
	}
	if resp := c.do(http.MethodPost, "/api/v1/work/"+a.ID+"/transitions", `{"to":"in_progress"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "unfinished_dependencies" {
		t.Errorf("start with a dependency: %d %q", resp.StatusCode, e.Error.Code)
	}
	if resp := c.do(http.MethodDelete, "/api/v1/work/"+a.ID+"/dependencies/"+b.ID, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete dependency: %d", resp.StatusCode)
	}
	if resp := c.do(http.MethodDelete, "/api/v1/work/"+a.ID+"/dependencies/"+b.ID, "", &e); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing dependency: %d", resp.StatusCode)
	}

	// Transitions.
	if resp := c.do(http.MethodPost, "/api/v1/work/"+b.ID+"/transitions", `{"to":"completed"}`, &e); resp.StatusCode != http.StatusConflict ||
		e.Error.Code != "illegal_transition" || e.Error.CurrentState != work.Design || strings.Join(e.Error.Allowed, ",") != "ready,blocked,wont_fix" {
		t.Errorf("illegal transition: %d %+v", resp.StatusCode, e.Error)
	}
	if resp := c.do(http.MethodPost, "/api/v1/work/"+b.ID+"/transitions", `{"to":"blocked","reason":"waiting on legal"}`, &got); resp.StatusCode != http.StatusOK ||
		got.State != work.Blocked || got.StateReason != "waiting on legal" {
		t.Errorf("transition: %d %+v", resp.StatusCode, got)
	}

	var events struct {
		Events []struct {
			Type    string `json:"type"`
			ToState string `json:"to_state"`
		} `json:"events"`
	}
	c.do(http.MethodGet, "/api/v1/work/"+b.ID+"/events", "", &events)
	if n := len(events.Events); n != 2 || events.Events[1].ToState != work.Blocked {
		t.Errorf("events = %+v", events.Events)
	}
	var outputs struct {
		Outputs []json.RawMessage `json:"outputs"`
	}
	if resp := c.do(http.MethodGet, "/api/v1/work/"+b.ID+"/outputs", "", &outputs); resp.StatusCode != http.StatusOK || outputs.Outputs == nil {
		t.Errorf("outputs: %d %+v, want 200 and an empty list", resp.StatusCode, outputs)
	}
}

func TestResponsesAreJSON(t *testing.T) {
	c := newServer(t, nil)
	resp := c.do(http.MethodGet, "/api/v1/work", "", nil)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}
