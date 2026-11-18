package api_test

import (
	"net/http"
	"testing"
)

type worker struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func TestWorkerProtocol(t *testing.T) {
	c := newServer(t, nil)
	var a, b worker
	if resp := c.do(http.MethodPost, "/api/v1/workers", `{"name":"a","kind":"cluster","capabilities":{"harnesses":["claude"],"labels":["gpu"]},"version":"1.0"}`, &a); resp.StatusCode != http.StatusOK || a.ID == "" {
		t.Fatalf("register a: %d %+v", resp.StatusCode, a)
	}
	c.do(http.MethodPost, "/api/v1/workers", `{"name":"b","kind":"local"}`, &b)
	var e apiError
	if resp := c.do(http.MethodPost, "/api/v1/workers", `{"name":"c","kind":"laptop"}`, &e); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad kind: %d", resp.StatusCode)
	}

	if resp := c.do(http.MethodPost, "/api/v1/workers/"+a.ID+"/claim", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("claim with nothing ready: %d, want 204", resp.StatusCode)
	}
	gpu := c.enqueue(`{"title":"needs gpu","requires":["gpu"]}`).Item
	if resp := c.do(http.MethodPost, "/api/v1/workers/"+b.ID+"/claim", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("b without a gpu: %d, want 204", resp.StatusCode)
	}
	var claimed item
	if resp := c.do(http.MethodPost, "/api/v1/workers/"+a.ID+"/claim", "", &claimed); resp.StatusCode != http.StatusOK || claimed.ID != gpu.ID || claimed.State != "in_progress" {
		t.Fatalf("a claims: %d %+v", resp.StatusCode, claimed)
	}

	var hb struct {
		Held         []string `json:"held"`
		LeaseSeconds float64  `json:"lease_seconds"`
	}
	if resp := c.do(http.MethodPost, "/api/v1/workers/"+a.ID+"/heartbeat", "", &hb); resp.StatusCode != http.StatusOK || len(hb.Held) != 1 || hb.LeaseSeconds <= 0 {
		t.Fatalf("heartbeat: %d %+v", resp.StatusCode, hb)
	}

	path := "/api/v1/work/" + gpu.ID
	if resp := c.do(http.MethodPost, path+"/stage", `{"worker_id":"`+b.ID+`","stage":"x"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_claimant" {
		t.Errorf("stage by b: %d %q, want 409 not_claimant", resp.StatusCode, e.Error.Code)
	}
	if resp := c.do(http.MethodPost, path+"/stage", `{"worker_id":"`+a.ID+`","stage":"pr_opened","outputs":[{"repo":"o/a","kind":"pull_request","url":"https://x/pull/1","status":"open"}]}`, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("stage by a: %d", resp.StatusCode)
	}
	var out struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if resp := c.do(http.MethodPost, path+"/outputs", `{"worker_id":"`+a.ID+`","repo":"o/b","kind":"commit","sha":"abc"}`, &out); resp.StatusCode != http.StatusOK || out.Status != "merged" {
		t.Errorf("record output: %d %+v", resp.StatusCode, out)
	}
	var result item
	if resp := c.do(http.MethodPost, path+"/result", `{"worker_id":"`+a.ID+`","outcome":"completed"}`, &result); resp.StatusCode != http.StatusOK || result.State != "in_review" {
		t.Fatalf("result: %d %+v, want in_review while the pull request is open", resp.StatusCode, result)
	}
	if resp := c.do(http.MethodPost, path+"/release", `{"worker_id":"`+a.ID+`"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_claimant" {
		t.Errorf("release after the claim ended: %d %q", resp.StatusCode, e.Error.Code)
	}
	// The merge arrives without a worker, as an adapter or operator would send it.
	if resp := c.do(http.MethodPost, path+"/outputs", `{"repo":"o/a","kind":"pull_request","url":"https://x/pull/1","status":"merged"}`, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("record merge: %d", resp.StatusCode)
	}
	c.do(http.MethodGet, path, "", &result)
	if result.State != "completed" {
		t.Errorf("after the merge: %s, want completed", result.State)
	}

	// Specific claim, release, and a failure result.
	other := c.enqueue(`{"title":"other"}`).Item
	opath := "/api/v1/work/" + other.ID
	if resp := c.do(http.MethodPost, opath+"/claim", `{"worker_id":"`+b.ID+`"}`, &claimed); resp.StatusCode != http.StatusOK || claimed.State != "in_progress" {
		t.Fatalf("claim specific: %d %+v", resp.StatusCode, claimed)
	}
	if resp := c.do(http.MethodPost, opath+"/claim", `{"worker_id":"`+a.ID+`"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "illegal_transition" {
		t.Errorf("claim a claimed item: %d %q", resp.StatusCode, e.Error.Code)
	}
	if resp := c.do(http.MethodPost, opath+"/release", `{"worker_id":"`+b.ID+`","reason":"going home"}`, &result); resp.StatusCode != http.StatusOK || result.State != "ready" {
		t.Errorf("release: %d %+v", resp.StatusCode, result)
	}
	c.do(http.MethodPost, opath+"/claim", `{"worker_id":"`+b.ID+`"}`, nil)
	if resp := c.do(http.MethodPost, opath+"/result", `{"worker_id":"`+b.ID+`","outcome":"failed","reason":"no such repo"}`, &result); resp.StatusCode != http.StatusOK || result.State != "failed" {
		t.Errorf("failed result: %d %+v", resp.StatusCode, result)
	}
	if resp := c.do(http.MethodPost, opath+"/result", `{"worker_id":"`+b.ID+`","outcome":"bogus"}`, &e); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad outcome: %d", resp.StatusCode)
	}

	var list struct {
		Workers []worker `json:"workers"`
	}
	if resp := c.do(http.MethodGet, "/api/v1/workers", "", &list); resp.StatusCode != http.StatusOK || len(list.Workers) != 2 {
		t.Errorf("list workers: %d %+v", resp.StatusCode, list)
	}
	if resp := c.do(http.MethodPost, "/api/v1/workers/00000000-0000-0000-0000-000000000000/heartbeat", "", &e); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown worker heartbeat: %d", resp.StatusCode)
	}
}
