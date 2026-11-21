package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/autobutler-org/factory/internal/server"
)

func get(t *testing.T, h http.Handler, method, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestUI(t *testing.T) {
	h := server.UI(fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"assets/app-1.css": {Data: []byte("body{}")},
	})
	cases := []struct {
		name, method, path string
		status             int
		body, cache        string
	}{
		{"root", http.MethodGet, "/", http.StatusOK, "app", "no-cache"},
		{"client route", http.MethodGet, "/work/0b6f0d1e-6d5c-4d8b-9a62-1f0f3c1d2e3f", http.StatusOK, "app", "no-cache"},
		{"nested client route", http.MethodGet, "/workers/", http.StatusOK, "app", "no-cache"},
		{"asset", http.MethodGet, "/assets/app-1.js", http.StatusOK, "console.log(1)", "public, max-age=31536000, immutable"},
		{"missing asset is not the app", http.MethodGet, "/assets/gone-2.js", http.StatusNotFound, "", ""},
		{"missing file with an extension", http.MethodGet, "/favicon.ico", http.StatusNotFound, "", ""},
		{"head", http.MethodHead, "/board", http.StatusOK, "", "no-cache"},
		{"post", http.MethodPost, "/", http.StatusMethodNotAllowed, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, h, tc.method, tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d (body %q)", resp.StatusCode, tc.status, body)
			}
			if !strings.Contains(body, tc.body) {
				t.Errorf("body %q, want it to contain %q", body, tc.body)
			}
			if tc.cache != "" && resp.Header.Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control %q, want %q", resp.Header.Get("Cache-Control"), tc.cache)
			}
		})
	}
}

func TestUIWithoutBuild(t *testing.T) {
	h := server.UI(fstest.MapFS{"stub.txt": {Data: []byte("x")}})
	for _, path := range []string{"/", "/work/x"} {
		resp, body := get(t, h, http.MethodGet, path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "make build/web") {
			t.Errorf("%s: %d %q, want a page that says to run make build/web", path, resp.StatusCode, body)
		}
	}
}

func TestPublic(t *testing.T) {
	// The embedded directory always holds at least the stub, so a Go build
	// without a web build still compiles and serves the placeholder.
	resp, _ := get(t, server.UI(server.Public()), http.MethodGet, "/")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("embedded UI: %d, want 200", resp.StatusCode)
	}
}
