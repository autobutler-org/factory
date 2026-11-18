package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

var adapter = work.Actor{Kind: work.ActorAdapter, ID: "fake-forge"}

func claimStep(t *testing.T, svc *work.Service, workerID, sandboxKey string) (work.StepClaim, bool) {
	t.Helper()
	c, ok, err := svc.ClaimStep(context.Background(), workerID, sandboxKey)
	if err != nil {
		t.Fatal(err)
	}
	return c, ok
}

// mustClaim claims and checks which step came back.
func mustClaim(t *testing.T, svc *work.Service, workerID, wantKey string) work.StepClaim {
	t.Helper()
	c, ok := claimStep(t, svc, workerID, "")
	if !ok || c.Step.Key != wantKey {
		t.Fatalf("claimed %q (%v), want %q", c.Step.Key, ok, wantKey)
	}
	return c
}

func states(t *testing.T, svc *work.Service, itemID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	g, err := svc.Graph(context.Background(), itemID)
	if errors.Is(err, work.ErrNotFound) {
		return out // not planned yet
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range g.Steps {
		out[s.Key] = s.State
	}
	return out
}

func step(t *testing.T, svc *work.Service, itemID, key string) db.WorkStep {
	t.Helper()
	g, err := svc.Graph(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range g.Steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no step %s", key)
	return db.WorkStep{}
}

func itemState(t *testing.T, svc *work.Service, id string) string {
	t.Helper()
	item, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return item.State
}

// fakeResult is what the scripted fake worker reports for a step: a pass
// verdict for reviews, the pull request for open_pr, the merge for merge.
func fakeResult(c work.StepClaim) work.StepResult {
	var p struct {
		Repo string `json:"repo"`
	}
	_ = json.Unmarshal(c.Step.Params, &p)
	url := "https://forge.test/" + p.Repo + "/pull/1"
	switch c.Step.Kind {
	case plan.Review:
		return succeed(`{"verdict":"pass"}`)
	case plan.OpenPR:
		r := succeed(`{"url":"` + url + `"}`)
		r.Outputs = []work.OutputInput{{Repo: p.Repo, Kind: work.KindPullRequest, URL: url, Branch: "factory/x", Status: work.OutputOpen}}
		return r
	case plan.Merge:
		r := succeed(`{"merged":true,"queued":false}`)
		r.Outputs = []work.OutputInput{{Repo: p.Repo, Kind: work.KindPullRequest, URL: url, Status: work.OutputMerged}}
		return r
	}
	return succeed(`{}`)
}

func succeed(result string) work.StepResult {
	return work.StepResult{Outcome: work.StepSucceeded, Result: json.RawMessage(result)}
}

func report(t *testing.T, svc *work.Service, c work.StepClaim, workerID string, r work.StepResult) db.WorkStep {
	t.Helper()
	s, err := svc.ReportStepResult(context.Background(), c.Step.ID, workerID, r)
	if err != nil {
		t.Fatalf("report %s: %v", c.Step.Key, err)
	}
	return s
}

// runUntil claims and completes steps with fakeResult, one at a time, until
// the step named stop is ready or nothing more can be claimed.
func runUntil(t *testing.T, svc *work.Service, workerID, itemID, stop string) {
	t.Helper()
	for range 100 {
		if stop != "" && states(t, svc, itemID)[stop] == plan.Ready {
			return
		}
		c, ok := claimStep(t, svc, workerID, "")
		if !ok {
			return
		}
		report(t, svc, c, workerID, fakeResult(c))
	}
	t.Fatal("runaway graph")
}

func TestClaimPlansTheItem(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	if _, err := svc.Graph(ctx, item.ID); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("graph before planning: %v, want ErrNotFound", err)
	}
	c := mustClaim(t, svc, w.ID, "o/a/workspace")
	if c.Item.ID != item.ID || c.Item.State != work.InProgress || c.Step.State != plan.Running || c.Step.SandboxKey != item.ID+"/o/a" {
		t.Fatalf("claim: %+v", c)
	}
	if !strings.Contains(string(c.Step.Params), `"branch": "factory/`+item.ID) {
		t.Errorf("workspace params: %s", c.Step.Params)
	}
	g, err := svc.Graph(ctx, item.ID)
	if err != nil || g.Graph.Version != 1 || g.Graph.Template != "default" || g.Graph.InputsHash == "" || len(g.Edges) != len(g.Steps)-1 {
		t.Fatalf("graph: %+v, %v", g.Graph, err)
	}
	types := eventTypes(t, svc, item.ID)
	if count(types, work.EventPlanned) != 1 || count(types, work.EventStepClaimed) != 1 {
		t.Errorf("events: %v", types)
	}

	// An item that cannot be planned is blocked with the reason.
	empty := mustCreate(t, svc, work.NewItem{Title: "no repos", Priority: 9})
	if _, ok := claimStep(t, svc, w.ID, ""); ok {
		t.Fatal("claimed a step while the only other item cannot be planned")
	}
	if got, _ := svc.Get(ctx, empty.ID); got.State != work.Blocked || !strings.Contains(got.StateReason, "no repositories") {
		t.Fatalf("unplannable item: %s %q", got.State, got.StateReason)
	}
}

func TestTwoChainsRunAtOnceButNeverOverlapWithinAChain(t *testing.T) {
	svc, _ := newService(t)
	a, b, c := register(t, svc, "a"), register(t, svc, "b"), register(t, svc, "c")
	mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/one", "o/two"}})

	first := mustClaim(t, svc, a.ID, "o/one/workspace")
	second := mustClaim(t, svc, b.ID, "o/two/workspace")
	if first.Step.SandboxKey == second.Step.SandboxKey {
		t.Fatal("two chains share a sandbox")
	}
	if got, ok := claimStep(t, svc, c.ID, ""); ok {
		t.Fatalf("c claimed %s while both chains have a running step", got.Step.Key)
	}
	report(t, svc, first, a.ID, fakeResult(first))
	// A sandbox started for chain two takes only chain two's steps.
	if got, ok := claimStep(t, svc, c.ID, second.Step.SandboxKey); ok {
		t.Fatalf("restricted claim got %s while chain two is busy", got.Step.Key)
	}
	next, ok := claimStep(t, svc, c.ID, first.Step.SandboxKey)
	if !ok || next.Step.Key != "o/one/implement" {
		t.Fatalf("restricted claim: %q %v", next.Step.Key, ok)
	}
	if len(next.Results) != 1 || string(next.Results["o/one/workspace"]) != "{}" {
		t.Errorf("claim carries prior results: %v", next.Results)
	}
}

// TestFakeWorkersDriveTwoChains runs several fake workers at once over the
// service until the item completes, and checks that no two steps of one
// sandbox ever ran together while the two chains did overlap.
func TestFakeWorkersDriveTwoChains(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/one", "o/two"}})

	var mu sync.Mutex
	running := map[string]bool{}
	overlapped := false
	deadline := time.Now().Add(30 * time.Second)
	var wg sync.WaitGroup
	for i := range 4 {
		w := register(t, svc, fmt.Sprint("w", i))
		wg.Go(func() {
			for time.Now().Before(deadline) {
				if itemState(t, svc, item.ID) == work.Completed {
					return
				}
				c, ok, err := svc.ClaimStep(ctx, w.ID, "")
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					completeReadyWaits(t, svc, item.ID)
					time.Sleep(5 * time.Millisecond)
					continue
				}
				mu.Lock()
				if running[c.Step.SandboxKey] {
					t.Errorf("%s started while another step of %s was running", c.Step.Key, c.Step.SandboxKey)
				}
				if c.Step.SandboxKey != "" {
					running[c.Step.SandboxKey] = true
				}
				busy := 0
				for _, on := range running {
					if on {
						busy++
					}
				}
				overlapped = overlapped || busy > 1
				mu.Unlock()
				time.Sleep(50 * time.Millisecond) // long enough for another chain to start
				mu.Lock()
				running[c.Step.SandboxKey] = false
				mu.Unlock()
				if _, err := svc.ReportStepResult(ctx, c.Step.ID, w.ID, fakeResult(c)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := itemState(t, svc, item.ID); got != work.Completed {
		t.Fatalf("item %s, want completed; steps %v", got, states(t, svc, item.ID))
	}
	if !overlapped {
		t.Error("the two chains never ran at the same time")
	}
}

// completeReadyWaits plays the adapter: checks pass for every waiting step.
func completeReadyWaits(t *testing.T, svc *work.Service, itemID string) {
	g, err := svc.Graph(context.Background(), itemID)
	if errors.Is(err, work.ErrNotFound) {
		return // not planned yet
	}
	if err != nil {
		t.Error(err)
		return
	}
	for _, s := range g.Steps {
		if plan.IsWait(s.Kind) && s.State == plan.Ready {
			_, err := svc.CompleteWait(context.Background(), s.ID, succeed(`{"verdict":"pass"}`), adapter)
			if err != nil && !errors.Is(err, work.ErrNotWaiting) {
				t.Error(err)
			}
		}
	}
}

func TestKilledWorkerReturnsOnlyItsStep(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	a, b := register(t, svc, "a"), register(t, svc, "b")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	ws := mustClaim(t, svc, a.ID, "o/a/workspace")
	report(t, svc, ws, a.ID, succeed(`{"branch":"factory/x"}`))
	impl := mustClaim(t, svc, a.ID, "o/a/implement")
	if held, err := svc.Heartbeat(ctx, a.ID); err != nil || len(held) != 1 || held[0] != impl.Step.ID {
		t.Fatalf("heartbeat: %v %v, want the step held", held, err)
	}

	// a is killed: its lease runs out.
	if _, err := pool.Exec(ctx, `UPDATE work_steps SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, impl.Step.ID); err != nil {
		t.Fatal(err)
	}
	r, err := svc.Reap(ctx)
	if err != nil || r.StepsExpired != 1 {
		t.Fatalf("reap: %+v %v", r, err)
	}
	got := states(t, svc, item.ID)
	if got["o/a/workspace"] != plan.Succeeded || got["o/a/implement"] != plan.Ready || got["o/a/gate"] != plan.Pending {
		t.Fatalf("after the kill: %v", got)
	}
	if s := step(t, svc, item.ID, "o/a/implement"); s.Attempts != 0 || s.ClaimedBy != nil {
		t.Errorf("expired step: attempts %d claimed by %v", s.Attempts, s.ClaimedBy)
	}
	again := mustClaim(t, svc, b.ID, "o/a/implement")
	if string(again.Results["o/a/workspace"]) != `{"branch": "factory/x"}` {
		t.Errorf("resume carries the workspace result: %s", again.Results["o/a/workspace"])
	}
	if _, err := svc.ReportStepResult(ctx, impl.Step.ID, a.ID, succeed(`{}`)); !errors.Is(err, lease.ErrNotClaimant) {
		t.Fatalf("late report from the killed worker: %v, want ErrNotClaimant", err)
	}
	if itemState(t, svc, item.ID) != work.InProgress {
		t.Fatal("the item left in_progress")
	}
	if n := count(eventTypes(t, svc, item.ID), work.EventStepLeaseExpired); n != 1 {
		t.Errorf("%d step_lease_expired events", n)
	}
}

func TestStuckStepsCountAttempts(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	svc = svc.WithPolicy(lease.Policy{Lease: time.Minute, StuckAfter: time.Hour, MaxAttempts: 2, RetryDelay: time.Minute})
	w := register(t, svc, "w")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	for attempt := 1; attempt <= 2; attempt++ {
		c := mustClaim(t, svc, w.ID, "o/a/workspace")
		if _, err := pool.Exec(ctx, `UPDATE work_steps SET stage_reported_at = now() - interval '2 hours' WHERE id = $1`, c.Step.ID); err != nil {
			t.Fatal(err)
		}
		if r, err := svc.Reap(ctx); err != nil || r.StepsStuck != 1 {
			t.Fatalf("reap: %+v %v", r, err)
		}
		if attempt == 1 {
			if _, ok := claimStep(t, svc, w.ID, ""); ok {
				t.Fatal("a retried step was handed out before its retry delay")
			}
			if _, err := pool.Exec(ctx, `UPDATE work_steps SET available_at = now() WHERE id = $1`, c.Step.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, _ := svc.Get(ctx, item.ID); got.State != work.Failed || !strings.Contains(got.StateReason, "o/a/workspace") {
		t.Fatalf("after two stuck attempts: %s %q", got.State, got.StateReason)
	}
	if got := states(t, svc, item.ID); got["o/a/workspace"] != plan.Failed || got["o/a/implement"] != plan.Canceled {
		t.Fatalf("steps: %v", got)
	}
}

func TestWaitStepIsCompletedByAnEvent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	runUntil(t, svc, w.ID, item.ID, "o/a/await_checks-1")
	if got := itemState(t, svc, item.ID); got != work.InReview {
		t.Fatalf("waiting on checks with the pull request open: %s, want in_review", got)
	}
	if c, ok := claimStep(t, svc, w.ID, ""); ok {
		t.Fatalf("a worker was handed the wait step %s", c.Step.Key)
	}
	checks := step(t, svc, item.ID, "o/a/await_checks-1")
	if _, err := svc.CompleteWait(ctx, step(t, svc, item.ID, "o/a/merge").ID, succeed(`{}`), adapter); !errors.Is(err, work.ErrNotWaiting) {
		t.Fatalf("completing a worker step: %v, want ErrNotWaiting", err)
	}
	if _, err := svc.CompleteWait(ctx, checks.ID, work.StepResult{Outcome: "maybe"}, adapter); !errors.Is(err, work.ErrInvalid) {
		t.Fatalf("bad outcome: %v, want ErrInvalid", err)
	}

	// The checks fail once: the fix round runs, then the second wait.
	if _, err := svc.CompleteWait(ctx, checks.ID, succeed(`{"verdict":"fail"}`), adapter); err != nil {
		t.Fatal(err)
	}
	fix := mustClaim(t, svc, w.ID, "o/a/fix-checks-1")
	if string(fix.Results["o/a/await_checks-1"]) != `{"verdict": "fail"}` {
		t.Errorf("fix sees the findings: %s", fix.Results["o/a/await_checks-1"])
	}
	report(t, svc, fix, w.ID, succeed(`{}`))
	second := step(t, svc, item.ID, "o/a/await_checks-2")
	if second.State != plan.Ready {
		t.Fatalf("await_checks-2: %s", second.State)
	}
	if _, err := svc.CompleteWait(ctx, second.ID, succeed(`{"verdict":"pass"}`), adapter); err != nil {
		t.Fatal(err)
	}
	// No reaper or scheduler pass ran: the merge is ready at once.
	merge := mustClaim(t, svc, w.ID, "o/a/merge")
	report(t, svc, merge, w.ID, fakeResult(merge))
	if got := itemState(t, svc, item.ID); got != work.Completed {
		t.Fatalf("after the merge: %s", got)
	}
	if _, err := svc.CompleteWait(ctx, second.ID, succeed(`{}`), adapter); !errors.Is(err, work.ErrNotWaiting) {
		t.Fatalf("completing twice: %v, want ErrNotWaiting", err)
	}
}

func TestReviewRounds(t *testing.T) {
	svc, _ := newService(t)
	w := register(t, svc, "w")
	rounds := []string{"o/a/review-1", "o/a/fix-1", "o/a/review-2", "o/a/fix-2", "o/a/review-3"}

	passed := mustCreate(t, svc, work.NewItem{Title: "passes at once", Repos: []string{"o/a"}})
	runUntil(t, svc, w.ID, passed.ID, "o/a/await_checks-1")
	got := states(t, svc, passed.ID)
	if got["o/a/review-1"] != plan.Succeeded {
		t.Fatalf("review-1: %s", got["o/a/review-1"])
	}
	for _, k := range rounds[1:] {
		if got[k] != plan.Skipped {
			t.Errorf("%s: %s, want skipped", k, got[k])
		}
	}

	// Changes twice, then the last round still asks for changes: failed.
	exhausted := mustCreate(t, svc, work.NewItem{Title: "never passes", Repos: []string{"o/b"}})
	runUntil(t, svc, w.ID, exhausted.ID, "o/b/review-1")
	for _, k := range []string{"o/b/review-1", "o/b/fix-1", "o/b/review-2", "o/b/fix-2", "o/b/review-3"} {
		c := mustClaim(t, svc, w.ID, k)
		r := succeed(`{}`)
		if c.Step.Kind == plan.Review {
			r = succeed(`{"verdict":"changes","findings":"rename it"}`)
		}
		report(t, svc, c, w.ID, r)
	}
	item, _ := svc.Get(context.Background(), exhausted.ID)
	if item.State != work.Failed || !strings.Contains(item.StateReason, "last round") {
		t.Fatalf("after three rounds of changes: %s %q", item.State, item.StateReason)
	}
	if got := states(t, svc, exhausted.ID); got["o/b/review-3"] != plan.Failed || got["o/b/merge"] != plan.Canceled {
		t.Fatalf("steps: %v", got)
	}
}

func TestDerivedItemState(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	svc = svc.WithPolicy(lease.Policy{Lease: time.Minute, StuckAfter: time.Hour, MaxAttempts: 2, RetryDelay: 0})
	w := register(t, svc, "w")

	// ready -> in_progress at the first claim -> in_review once the last
	// open_pr has finished -> completed when the merge lands.
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a", "o/b"}})
	ws := mustClaim(t, svc, w.ID, "o/a/workspace")
	if itemState(t, svc, item.ID) != work.InProgress {
		t.Fatal("not in_progress after the first claim")
	}
	report(t, svc, ws, w.ID, succeed(`{}`))
	runUntil(t, svc, w.ID, item.ID, "o/a/await_checks-1")
	if got := itemState(t, svc, item.ID); got != work.InProgress {
		t.Fatalf("o/b/open_pr still to run, but the item is %s", got)
	}
	runUntil(t, svc, w.ID, item.ID, "o/b/await_checks-1")
	if got := itemState(t, svc, item.ID); got != work.InReview {
		t.Fatalf("both pull requests open: %s, want in_review", got)
	}
	completeReadyWaits(t, svc, item.ID)
	runUntil(t, svc, w.ID, item.ID, "")
	if got := itemState(t, svc, item.ID); got != work.Completed {
		t.Fatalf("after both merges: %s; steps %v", got, states(t, svc, item.ID))
	}
	for k, s := range states(t, svc, item.ID) {
		if s != plan.Succeeded && s != plan.Skipped {
			t.Errorf("%s ended %s", k, s)
		}
	}

	// A retryable failure retries the step; a second one fails the item.
	flaky := mustCreate(t, svc, work.NewItem{Title: "flaky", Repos: []string{"o/c"}})
	c := mustClaim(t, svc, w.ID, "o/c/workspace")
	s := report(t, svc, c, w.ID, work.StepResult{Outcome: work.StepFailed, Retryable: true, Reason: "network"})
	if s.State != plan.Ready || s.Attempts != 1 || itemState(t, svc, flaky.ID) != work.InProgress {
		t.Fatalf("after a retryable failure: %s attempts %d", s.State, s.Attempts)
	}
	c = mustClaim(t, svc, w.ID, "o/c/workspace")
	report(t, svc, c, w.ID, work.StepResult{Outcome: work.StepFailed, Retryable: true, Reason: "network"})
	if got, _ := svc.Get(ctx, flaky.ID); got.State != work.Failed || !strings.Contains(got.StateReason, "network") {
		t.Fatalf("at the attempt limit: %s %q", got.State, got.StateReason)
	}

	// Blocked: the step needs a decision.
	stuck := mustCreate(t, svc, work.NewItem{Title: "unclear", Repos: []string{"o/d"}})
	c = mustClaim(t, svc, w.ID, "o/d/workspace")
	report(t, svc, c, w.ID, succeed(`{}`))
	c = mustClaim(t, svc, w.ID, "o/d/implement")
	report(t, svc, c, w.ID, work.StepResult{Outcome: work.StepBlocked, Reason: "which API version?"})
	if got, _ := svc.Get(ctx, stuck.ID); got.State != work.Blocked || got.StateReason != "which API version?" {
		t.Fatalf("blocked: %s %q", got.State, got.StateReason)
	}
	if got := states(t, svc, stuck.ID); got["o/d/implement"] != plan.Failed || got["o/d/gate"] != plan.Canceled {
		t.Fatalf("steps after blocked: %v", got)
	}

	// Unblocked and taken again: a new graph version.
	if _, err := svc.Transition(ctx, stuck.ID, work.Ready, "answered", user); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, svc, w.ID, "o/d/workspace")
	if g, _ := svc.Graph(ctx, stuck.ID); g.Graph.Version != 2 {
		t.Fatalf("version %d after re-taking, want 2", g.Graph.Version)
	}
}

func TestStageReleaseAndOnlyTheClaimant(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	a, b := register(t, svc, "a"), register(t, svc, "b")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	c := mustClaim(t, svc, a.ID, "o/a/workspace")
	if _, err := svc.ReportStepStage(ctx, c.Step.ID, b.ID, "x", nil); !errors.Is(err, lease.ErrNotClaimant) {
		t.Fatalf("stage by b: %v", err)
	}
	if _, err := svc.ReleaseStep(ctx, c.Step.ID, b.ID, ""); !errors.Is(err, lease.ErrNotClaimant) {
		t.Fatalf("release by b: %v", err)
	}
	s, err := svc.ReportStepStage(ctx, c.Step.ID, a.ID, "cloned", []work.OutputInput{{Repo: "o/a", Kind: work.KindCommit, SHA: "abc"}})
	if err != nil || s.Stage != "cloned" {
		t.Fatalf("stage: %+v %v", s, err)
	}
	if outs, _ := svc.Outputs(ctx, item.ID); len(outs) != 1 {
		t.Fatalf("outputs: %v", outs)
	}
	s, err = svc.ReleaseStep(ctx, c.Step.ID, a.ID, "shutting down")
	if err != nil || s.State != plan.Ready || s.Attempts != 0 || s.ClaimedBy != nil {
		t.Fatalf("release: %+v %v", s, err)
	}
	if _, err := svc.ReportStepResult(ctx, c.Step.ID, a.ID, work.StepResult{Outcome: "done"}); !errors.Is(err, work.ErrInvalid) {
		t.Fatalf("bad outcome: %v", err)
	}
	mustClaim(t, svc, b.ID, "o/a/workspace")
}

func TestLocalClaimHandsOverTheGraph(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	cluster := register(t, svc, "cluster")
	dev, err := svc.RegisterWorker(ctx, work.WorkerInput{Name: "laptop", Kind: work.WorkerLocal})
	if err != nil {
		t.Fatal(err)
	}
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	c := mustClaim(t, svc, cluster.ID, "o/a/workspace")

	// An operator takes it back to ready; the running step is canceled and
	// the cluster worker loses it at its next heartbeat.
	if _, err := svc.Transition(ctx, item.ID, work.Ready, "taking it local", user); err != nil {
		t.Fatal(err)
	}
	if held, _ := svc.Heartbeat(ctx, cluster.ID); len(held) != 0 {
		t.Fatalf("still held: %v", held)
	}
	if _, err := svc.Claim(ctx, item.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Graph(ctx, item.ID)
	if err != nil || g.Graph.State != work.GraphHandedOver {
		t.Fatalf("graph: %+v %v", g.Graph, err)
	}
	if got := states(t, svc, item.ID); got["o/a/workspace"] != plan.Canceled || got["o/a/merge"] != plan.Canceled {
		t.Fatalf("steps: %v", got)
	}
	if got, ok := claimStep(t, svc, cluster.ID, ""); ok {
		t.Fatalf("the cluster claimed %s of a locally claimed item", got.Step.Key)
	}
	if _, err := svc.ReportStepResult(ctx, c.Step.ID, cluster.ID, succeed(`{}`)); !errors.Is(err, lease.ErrNotClaimant) {
		t.Fatalf("late step report: %v", err)
	}
}

func TestReplan(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	if _, err := svc.Replan(ctx, item.ID, user); !errors.Is(err, work.ErrNotReplannable) {
		t.Fatalf("replanning a ready item: %v", err)
	}
	c := mustClaim(t, svc, w.ID, "o/a/workspace")
	report(t, svc, c, w.ID, succeed(`{}`))
	g, err := svc.Replan(ctx, item.ID, user)
	if err != nil || g.Graph.Version != 2 || g.Graph.State != work.GraphCurrent {
		t.Fatalf("replan: %+v %v", g.Graph, err)
	}
	if got := states(t, svc, item.ID); got["o/a/workspace"] != plan.Ready || got["o/a/implement"] != plan.Pending {
		t.Fatalf("new graph: %v", got)
	}
	mustClaim(t, svc, w.ID, "o/a/workspace")
	if _, err := svc.Replan(ctx, "00000000-0000-0000-0000-000000000000", user); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
}
