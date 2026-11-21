package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/doctor"
)

func TestDoctorRoute(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(api.New(api.Config{
		Token: token, Version: "v0.3.0", Logger: slog.New(slog.DiscardHandler),
		Doctor: func(context.Context) []doctor.Result {
			return []doctor.Result{{Name: "database reachable and migrated", Status: doctor.Fail, Fix: "run it", Command: "factory migrate up"}}
		},
	}))
	t.Cleanup(srv.Close)
	c := &client{t: t, srv: srv}
	var got struct {
		Version string          `json:"version"`
		Checks  []doctor.Result `json:"checks"`
	}
	if resp := c.do(http.MethodGet, "/api/v1/admin/doctor", "", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with a failing check", resp.StatusCode)
	}
	if got.Version != "v0.3.0" || len(got.Checks) != 1 || got.Checks[0].Command != "factory migrate up" {
		t.Errorf("doctor = %+v", got)
	}
	resp, err := http.Get(srv.URL + "/api/v1/admin/doctor")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without the token: %d, want 401", resp.StatusCode)
	}
}
