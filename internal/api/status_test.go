package api_test

import (
	"net/http"
	"testing"
)

func TestStatusRoute(t *testing.T) {
	c := newServer(t, nil)
	c.do(http.MethodPost, "/api/v1/work", `{"title": "waiting"}`, nil)
	var st struct {
		Counts         map[string]int `json:"counts"`
		Workers        map[string]int `json:"workers"`
		OldestReady    *item          `json:"oldest_ready"`
		RecentFailures []item         `json:"recent_failures"`
	}
	if resp := c.do(http.MethodGet, "/api/v1/status", "", &st); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if st.Counts["ready"] != 1 || st.OldestReady == nil || st.OldestReady.Title != "waiting" || st.RecentFailures == nil || st.Workers == nil {
		t.Errorf("status = %+v, want one ready item, it as the oldest, and empty lists rather than null", st)
	}
}
