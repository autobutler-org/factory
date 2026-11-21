package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/autobutler-org/factory/internal/api"
)

// The UI is served without the token; the API still needs it.
func TestUIIsPublicAndAPIIsNot(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := api.New(api.Config{Token: token, Logger: slog.New(slog.DiscardHandler), UI: ui})
	cases := []struct {
		path   string
		status int
	}{
		{"/", http.StatusTeapot},
		{"/work/abc", http.StatusTeapot},
		{"/workers", http.StatusTeapot},
		{"/api/v1/work", http.StatusUnauthorized},
		{"/api/v1/nope", http.StatusUnauthorized},
		{"/healthz", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.status {
				t.Errorf("GET %s: %d, want %d", tc.path, rec.Code, tc.status)
			}
		})
	}
}

func TestNoUIAnswersJSONNotFound(t *testing.T) {
	h := api.New(api.Config{Token: token, Logger: slog.New(slog.DiscardHandler)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/work/abc", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("without a UI: %d %q, want a JSON 404", rec.Code, rec.Header().Get("Content-Type"))
	}
}
