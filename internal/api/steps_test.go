package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

type stepJSON struct {
	ID         string          `json:"id"`
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	SandboxKey string          `json:"sandbox_key"`
	Params     json.RawMessage `json:"params"`
	Attempts   int             `json:"attempts"`
}

type claimJSON struct {
	Step    stepJSON                   `json:"step"`
	Item    item                       `json:"item"`
	Results map[string]json.RawMessage `json:"results"`
	Outputs []json.RawMessage          `json:"outputs"`
}

type graphJSON struct {
	Graph struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		State   string `json:"state"`
	} `json:"graph"`
	Steps []stepJSON `json:"steps"`
	Edges []struct {
		StepID      string `json:"step_id"`
		DependsOnID string `json:"depends_on_id"`
	} `json:"edges"`
}

// fakeWorker is a scripted worker speaking the step protocol over HTTP.
type fakeWorker struct {
	c  *client
	id string
}

func (c *client) worker(name string, labels ...string) fakeWorker {
	c.t.Helper()
	caps, _ := json.Marshal(map[string]any{"labels": labels})
	var w worker
	if resp := c.do(http.MethodPost, "/api/v1/workers", `{"name":"`+name+`","kind":"cluster","capabilities":`+string(caps)+`}`, &w); resp.StatusCode != http.StatusOK {
		c.t.Fatalf("register %s: %d", name, resp.StatusCode)
	}
	return fakeWorker{c: c, id: w.ID}
}

// claim returns the claimed step, or ok false on 204.
func (w fakeWorker) claim(body string) (claimJSON, bool) {
	w.c.t.Helper()
	var out claimJSON
	resp := w.c.do(http.MethodPost, "/api/v1/workers/"+w.id+"/claim", body, &out)
	switch resp.StatusCode {
	case http.StatusOK:
		return out, true
	case http.StatusNoContent:
		return out, false
	}
	w.c.t.Fatalf("claim: %d", resp.StatusCode)
	return out, false
}

// run claims and succeeds steps with canned results until nothing is left.
func (w fakeWorker) run() {
	w.c.t.Helper()
	for range 50 {
		cl, ok := w.claim("")
		if !ok {
			return
		}
		body := `{"worker_id":"` + w.id + `","outcome":"succeeded","result":{}}`
		switch cl.Step.Kind {
		case "review":
			body = `{"worker_id":"` + w.id + `","outcome":"succeeded","result":{"verdict":"pass"}}`
		case "open_pr":
			body = `{"worker_id":"` + w.id + `","outcome":"succeeded","result":{"url":"https://x/pull/1"},` +
				`"outputs":[{"repo":"o/a","kind":"pull_request","url":"https://x/pull/1","status":"open"}]}`
		case "merge":
			body = `{"worker_id":"` + w.id + `","outcome":"succeeded","result":{"merged":true},` +
				`"outputs":[{"repo":"o/a","kind":"pull_request","url":"https://x/pull/1","status":"merged"}]}`
		}
		if resp := w.c.do(http.MethodPost, "/api/v1/steps/"+cl.Step.ID+"/result", body, nil); resp.StatusCode != http.StatusOK {
			w.c.t.Fatalf("result for %s: %d", cl.Step.Key, resp.StatusCode)
		}
	}
	w.c.t.Fatal("runaway graph")
}

func TestStepProtocol(t *testing.T) {
	c := newServer(t, nil)
	a, b := c.worker("a", "gpu"), c.worker("b")
	if _, ok := a.claim(""); ok {
		t.Fatal("claimed with nothing ready")
	}
	it := c.enqueue(`{"title":"t","repos":["o/a"],"requires":["gpu"]}`).Item
	path := "/api/v1/work/" + it.ID
	var e apiError
	if resp := c.do(http.MethodGet, path+"/graph", "", &e); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("graph before planning: %d", resp.StatusCode)
	}
	if _, ok := b.claim(""); ok {
		t.Fatal("b has no gpu")
	}
	cl, ok := a.claim(`{"sandbox_key":""}`)
	if !ok || cl.Step.Key != "o/a/workspace" || cl.Item.State != "in_progress" || cl.Step.SandboxKey != it.ID+"/o/a" || cl.Results == nil || cl.Outputs == nil {
		t.Fatalf("claim: %+v", cl)
	}
	step := "/api/v1/steps/" + cl.Step.ID
	if resp := c.do(http.MethodPost, step+"/stage", `{"worker_id":"`+b.id+`","stage":"x"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_claimant" {
		t.Errorf("stage by b: %d %q", resp.StatusCode, e.Error.Code)
	}
	var s stepJSON
	if resp := c.do(http.MethodPost, step+"/stage", `{"worker_id":"`+a.id+`","stage":"cloned"}`, &s); resp.StatusCode != http.StatusOK || s.State != "running" {
		t.Errorf("stage: %d %+v", resp.StatusCode, s)
	}
	var hb struct {
		Held []string `json:"held"`
	}
	if c.do(http.MethodPost, "/api/v1/workers/"+a.id+"/heartbeat", "", &hb); len(hb.Held) != 1 || hb.Held[0] != cl.Step.ID {
		t.Errorf("heartbeat holds %v", hb.Held)
	}
	if resp := c.do(http.MethodPost, step+"/release", `{"worker_id":"`+a.id+`","reason":"sigterm"}`, &s); resp.StatusCode != http.StatusOK || s.State != "ready" {
		t.Errorf("release: %d %+v", resp.StatusCode, s)
	}
	if resp := c.do(http.MethodPost, step+"/result", `{"worker_id":"`+a.id+`","outcome":"succeeded"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_claimant" {
		t.Errorf("result after release: %d %q", resp.StatusCode, e.Error.Code)
	}
	if resp := c.do(http.MethodPost, step+"/complete", `{"outcome":"succeeded"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_waiting" {
		t.Errorf("complete a worker step: %d %q", resp.StatusCode, e.Error.Code)
	}
	if resp := c.do(http.MethodPost, step+"/result", `{"worker_id":"`+a.id+`","outcome":"nope"}`, &e); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad outcome: %d", resp.StatusCode)
	}

	// The fake worker runs until the chain waits on checks.
	a.run()
	var g graphJSON
	if resp := c.do(http.MethodGet, path+"/graph", "", &g); resp.StatusCode != http.StatusOK || g.Graph.Version != 1 || len(g.Edges) != len(g.Steps)-1 {
		t.Fatalf("graph: %d %+v", resp.StatusCode, g.Graph)
	}
	var waiting stepJSON
	for _, st := range g.Steps {
		if st.State == "ready" {
			waiting = st
		}
	}
	if waiting.Kind != "await_checks" {
		t.Fatalf("waiting on %+v, want await_checks", waiting)
	}
	var got item
	if c.do(http.MethodGet, path, "", &got); got.State != "in_review" {
		t.Fatalf("waiting on checks: %s", got.State)
	}
	// The adapter's event completes the wait; the worker carries on.
	if resp := c.do(http.MethodPost, "/api/v1/steps/"+waiting.ID+"/complete", `{"outcome":"succeeded","result":{"verdict":"pass"}}`, &s); resp.StatusCode != http.StatusOK || s.State != "succeeded" {
		t.Fatalf("complete: %d %+v", resp.StatusCode, s)
	}
	a.run()
	if c.do(http.MethodGet, path, "", &got); got.State != "completed" {
		t.Fatalf("after the merge: %s", got.State)
	}
	if resp := c.do(http.MethodPost, path+"/replan", "", &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "not_replannable" {
		t.Errorf("replan a completed item: %d %q", resp.StatusCode, e.Error.Code)
	}

	other := c.enqueue(`{"title":"u","repos":["o/a"]}`).Item
	b.claim("")
	if resp := c.do(http.MethodPost, "/api/v1/work/"+other.ID+"/replan", "", &g); resp.StatusCode != http.StatusOK || g.Graph.Version != 2 {
		t.Fatalf("replan: %d %+v", resp.StatusCode, g.Graph)
	}
	if resp := c.do(http.MethodPost, "/api/v1/steps/00000000-0000-0000-0000-000000000000/result", `{"worker_id":"`+b.id+`","outcome":"succeeded"}`, &e); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown step: %d", resp.StatusCode)
	}
}
