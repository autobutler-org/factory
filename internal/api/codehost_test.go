package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/work"
)

func TestCodehostToken(t *testing.T) {
	var logs bytes.Buffer
	pool := dbtest.NewDB(t)
	expires := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(api.New(api.Config{
		Work:   work.New(pool),
		Ready:  func(context.Context) error { return nil },
		Token:  token,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		MintToken: func(_ context.Context, repo string) (codehost.Token, error) {
			return codehost.Token{Token: "ghs_secret_for_" + repo, ExpiresAt: expires}, nil
		},
	}))
	t.Cleanup(srv.Close)
	c := &client{t: t, srv: srv}

	var holder, other worker
	c.do(http.MethodPost, "/api/v1/workers", `{"name":"holder","kind":"cluster"}`, &holder)
	c.do(http.MethodPost, "/api/v1/workers", `{"name":"other","kind":"cluster"}`, &other)
	it := c.enqueue(`{"title":"x","repos":["o/a","o/b"]}`).Item
	path := "/api/v1/work/" + it.ID + "/codehost-token"
	// The holder claims a step, o/a's workspace; o/b's chain is untouched.
	var claim struct {
		Step struct {
			Key string `json:"key"`
		} `json:"step"`
	}
	if resp := c.do(http.MethodPost, "/api/v1/workers/"+holder.ID+"/claim", "", &claim); resp.StatusCode != http.StatusOK || claim.Step.Key != "o/a/workspace" {
		t.Fatalf("claim a step: %d %+v", resp.StatusCode, claim)
	}

	var got struct {
		Repo      string    `json:"repo"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp := c.do(http.MethodPost, path, `{"worker_id":"`+holder.ID+`","repo":"o/a"}`, &got); resp.StatusCode != http.StatusOK ||
		got.Token != "ghs_secret_for_o/a" || got.Repo != "o/a" || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("holder: %d %+v", resp.StatusCode, got)
	}
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"another worker", `{"worker_id":"` + other.ID + `","repo":"o/a"}`, "not_claimant", http.StatusConflict},
		{"the item's other repository", `{"worker_id":"` + holder.ID + `","repo":"o/b"}`, "repo_not_allowed", http.StatusForbidden},
		{"no repository", `{"worker_id":"` + holder.ID + `"}`, "invalid_request", http.StatusUnprocessableEntity},
		{"unknown field", `{"worker_id":"` + holder.ID + `","repo":"o/a","scope":"all"}`, "malformed_request", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e apiError
			if resp := c.do(http.MethodPost, path, tc.body, &e); resp.StatusCode != tc.status || e.Error.Code != tc.code {
				t.Errorf("%d %q, want %d %q", resp.StatusCode, e.Error.Code, tc.status, tc.code)
			}
		})
	}
	if strings.Contains(logs.String(), "ghs_secret") {
		t.Errorf("the log carries the token:\n%s", logs.String())
	}
}

func TestCodehostTokenWithoutAnApp(t *testing.T) {
	c := newServer(t, nil)
	it := c.enqueue(`{"title":"x","repos":["o/a"]}`).Item
	var e apiError
	if resp := c.do(http.MethodPost, "/api/v1/work/"+it.ID+"/codehost-token", `{"worker_id":"w","repo":"o/a"}`, &e); resp.StatusCode != http.StatusServiceUnavailable || e.Error.Code != "codehost_not_configured" {
		t.Errorf("%d %q, want 503 codehost_not_configured", resp.StatusCode, e.Error.Code)
	}
}
