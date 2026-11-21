package apiclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCall(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/created":
			if r.Header.Get("Idempotency-Key") != "k" || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("headers = %v", r.Header)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"a"}`))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/refused":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"illegal_transition","message":"no","current_state":"design","allowed":["ready","blocked"]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL + "/", Token: func() (string, error) { return "secret", nil }}
	ctx := t.Context()

	var out struct{ ID string }
	ok, err := c.Call(ctx, http.MethodPost, "/created", http.Header{"Idempotency-Key": {"k"}}, map[string]string{"a": "b"}, &out)
	if !ok || err != nil || out.ID != "a" {
		t.Errorf("201 = %v, %v, %+v; want ok and the body", ok, err, out)
	}
	if ok, err := c.Call(ctx, http.MethodGet, "/empty", nil, nil, &out); ok || err != nil {
		t.Errorf("204 = %v, %v; want not ok and no error", ok, err)
	}
	_, err = c.Call(ctx, http.MethodPost, "/refused", nil, nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.CurrentState != "design" || !slices.Equal(apiErr.Allowed, []string{"ready", "blocked"}) {
		t.Errorf("409 = %v (%+v), want an *Error with the allowed targets", err, apiErr)
	}

	dead := &Client{URL: "http://127.0.0.1:1"}
	if _, err := dead.Call(ctx, http.MethodGet, "/healthz", nil, nil, nil); !errors.Is(err, ErrUnreachable) {
		t.Errorf("dead server = %v, want ErrUnreachable", err)
	}
	failing := &Client{URL: srv.URL, Token: func() (string, error) { return "", errors.New("no token file") }}
	if _, err := failing.Call(ctx, http.MethodGet, "/empty", nil, nil, nil); err == nil {
		t.Error("a failing token source sent the request anyway")
	}
}
