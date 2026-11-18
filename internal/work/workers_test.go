package work_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/jackc/pgx/v5/pgxpool"
)

func register(t *testing.T, svc *work.Service, name string, caps ...string) db.Worker {
	t.Helper()
	w, err := svc.RegisterWorker(context.Background(), work.WorkerInput{Name: name, Kind: work.WorkerCluster, Capabilities: work.Capabilities{Labels: caps}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// age moves a column of an item into the past, standing in for the clock.
func age(t *testing.T, pool *pgxpool.Pool, id, column string, by time.Duration) {
	t.Helper()
	sql := fmt.Sprintf(`UPDATE work_items SET %s = %s - make_interval(secs => $2) WHERE id = $1`, column, column)
	if _, err := pool.Exec(context.Background(), sql, id, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func eventTypes(t *testing.T, svc *work.Service, id string) []string {
	t.Helper()
	events, err := svc.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func count(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

func TestRegisterAgainRefreshes(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	first := register(t, svc, "sandbox-1", "gpu")
	again, err := svc.RegisterWorker(ctx, work.WorkerInput{Name: "sandbox-1", Kind: work.WorkerCluster, Version: "v2"})
	if err != nil || again.ID != first.ID || again.Version != "v2" {
		t.Fatalf("re-register: %+v, %v; want the same id with the new version", again, err)
	}
	for _, bad := range []work.WorkerInput{{Kind: work.WorkerCluster}, {Name: "x", Kind: "laptop"}} {
		if _, err := svc.RegisterWorker(ctx, bad); !errors.Is(err, work.ErrInvalid) {
			t.Errorf("register %+v: %v, want ErrInvalid", bad, err)
		}
	}
	workers, err := svc.Workers(ctx)
	if err != nil || len(workers) != 1 {
		t.Fatalf("workers = %d, %v", len(workers), err)
	}
}

func TestFiftyClaimersTenItems(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	for i := range 10 {
		mustCreate(t, svc, work.NewItem{Title: fmt.Sprint("item ", i), Repos: []string{"o/a"}})
	}
	workers := make([]db.Worker, 50)
	for i := range workers {
		workers[i] = register(t, svc, fmt.Sprint("w", i))
	}

	var mu sync.Mutex
	claimed := map[string]string{}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			c, ok, err := svc.ClaimStep(ctx, w.ID, "")
			if err != nil {
				t.Error(err)
				return
			}
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if other, dup := claimed[c.Item.ID]; dup {
				t.Errorf("item %s claimed by %s and %s", c.Item.ID, other, w.ID)
			}
			claimed[c.Item.ID] = w.ID
		})
	}
	wg.Wait()
	if len(claimed) != 10 {
		t.Fatalf("%d items claimed, want all 10 exactly once", len(claimed))
	}
	for id, by := range claimed {
		item, err := svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ws := step(t, svc, id, "o/a/workspace")
		if item.State != work.InProgress || ws.ClaimedBy == nil || *ws.ClaimedBy != by {
			t.Errorf("item %s: state %s, workspace claimed by %v, want in_progress by %s", id, item.State, ws.ClaimedBy, by)
		}
		if n := count(eventTypes(t, svc, id), work.EventStepClaimed); n != 1 {
			t.Errorf("item %s has %d step claim events, want 1", id, n)
		}
	}
}

func TestClaimOrderCapabilitiesAndDependencies(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	repos := []string{"o/a"}
	low := mustCreate(t, svc, work.NewItem{Title: "low", Priority: 1, Repos: repos})
	high := mustCreate(t, svc, work.NewItem{Title: "high", Priority: 5, Repos: repos})
	gpu := mustCreate(t, svc, work.NewItem{Title: "needs gpu", Priority: 9, Requires: []string{"gpu"}, Repos: repos})
	blocker := mustCreate(t, svc, work.NewItem{Title: "blocker", State: work.Design})
	blocked := mustCreate(t, svc, work.NewItem{Title: "blocked", Priority: 8, Repos: repos})
	if err := svc.AddDependency(ctx, blocked.ID, blocker.ID, user); err != nil {
		t.Fatal(err)
	}
	plain := register(t, svc, "plain")

	// One worker per claim: a worker could take the next step of the item
	// it just claimed otherwise, but that step is not ready yet anyway.
	for i, want := range []string{high.ID, low.ID} {
		got, ok := claimStep(t, svc, register(t, svc, fmt.Sprint("plain", i)).ID, "")
		if !ok || got.Item.ID != want {
			t.Fatalf("claimed %q (%v), want %q", got.Item.Title, ok, want)
		}
	}
	if got, ok := claimStep(t, svc, plain.ID, ""); ok {
		t.Fatalf("claimed %q; the rest need a gpu, have an unfinished dependency, or have a step running", got.Item.Title)
	}
	if got, ok := claimStep(t, svc, register(t, svc, "gpu-box", "gpu").ID, ""); !ok || got.Item.ID != gpu.ID {
		t.Fatalf("gpu worker claimed %q (%v), want the gpu item", got.Item.Title, ok)
	}
	if _, err := svc.Claim(ctx, blocked.ID, plain.ID); !errors.Is(err, work.ErrUnfinishedDependencies) {
		t.Fatalf("claiming blocked work directly: %v, want ErrUnfinishedDependencies", err)
	}
	if _, err := svc.Claim(ctx, high.ID, plain.ID); !errors.Is(err, work.ErrIllegalTransition) {
		t.Fatalf("claiming claimed work: %v, want ErrIllegalTransition", err)
	}
	if _, _, err := svc.ClaimStep(ctx, "00000000-0000-0000-0000-000000000000", ""); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("unknown worker: %v, want ErrNotFound", err)
	}
}

func TestOnlyTheClaimantMayAct(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t"})
	a, b := register(t, svc, "a"), register(t, svc, "b")
	if _, err := svc.Claim(ctx, item.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func() error{
		"stage": func() error { _, err := svc.ReportStage(ctx, item.ID, b.ID, "x", nil); return err },
		"result": func() error {
			_, err := svc.ReportResult(ctx, item.ID, b.ID, work.Result{Outcome: work.Failed})
			return err
		},
		"release": func() error { _, err := svc.Release(ctx, item.ID, b.ID, ""); return err },
		"output": func() error {
			_, err := svc.RecordWorkerOutput(ctx, item.ID, b.ID, work.OutputInput{Repo: "r", Kind: work.KindCommit, SHA: "s"})
			return err
		},
	}
	for name, f := range checks {
		if err := f(); !errors.Is(err, lease.ErrNotClaimant) {
			t.Errorf("%s by another worker: %v, want ErrNotClaimant", name, err)
		}
	}
	got, _ := svc.Get(ctx, item.ID)
	if got.State != work.InProgress || *got.ClaimedBy != a.ID || got.Stage != "" {
		t.Fatalf("item changed: %+v", got)
	}
}

func TestLeaseExpiryTakeover(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t"})
	a, b := register(t, svc, "a"), register(t, svc, "b")
	if _, err := svc.Claim(ctx, item.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	held, err := svc.Heartbeat(ctx, a.ID)
	if err != nil || len(held) != 1 || held[0] != item.ID {
		t.Fatalf("heartbeat: %v, %v; want the item held", held, err)
	}

	// a goes quiet past its lease.
	age(t, pool, item.ID, "lease_expires_at", time.Hour)
	if _, err := svc.ReportStage(ctx, item.ID, a.ID, "late", nil); !errors.Is(err, lease.ErrExpired) {
		t.Fatalf("report after expiry: %v, want ErrExpired", err)
	}
	if held, _ := svc.Heartbeat(ctx, a.ID); len(held) != 0 {
		t.Fatalf("heartbeat revived an expired lease: %v", held)
	}
	report, err := svc.Reap(ctx)
	if err != nil || report.Expired != 1 {
		t.Fatalf("reap: %+v, %v; want one expired lease", report, err)
	}
	got, _ := svc.Get(ctx, item.ID)
	if got.State != work.Ready || got.ClaimedBy != nil || got.Attempts != 0 {
		t.Fatalf("after expiry: %s claimed by %v attempts %d; want ready, unclaimed, no attempt", got.State, got.ClaimedBy, got.Attempts)
	}

	if _, err := svc.Claim(ctx, item.ID, b.ID); err != nil {
		t.Fatalf("b did not take over the item: %v", err)
	}
	if mine, _, err := svc.List(ctx, work.Filter{ClaimedBy: b.ID}); err != nil || len(mine) != 1 {
		t.Fatalf("list claimed by b: %d, %v", len(mine), err)
	}
	if _, _, err := svc.List(ctx, work.Filter{ClaimedBy: "b"}); !errors.Is(err, work.ErrInvalid) {
		t.Fatalf("claimed_by that is not an id: %v, want ErrInvalid", err)
	}
	if _, err := svc.ReportResult(ctx, item.ID, a.ID, work.Result{Outcome: work.Failed, Reason: "late"}); !errors.Is(err, lease.ErrNotClaimant) {
		t.Fatalf("late result from a: %v, want ErrNotClaimant", err)
	}
	got, _ = svc.Get(ctx, item.ID)
	if got.State != work.InProgress || *got.ClaimedBy != b.ID || got.Attempts != 0 {
		t.Fatalf("the late report changed the item: %+v", got)
	}
	if n := count(eventTypes(t, svc, item.ID), work.EventLeaseExpired); n != 1 {
		t.Errorf("%d lease_expired events, want 1", n)
	}
}

func TestStuckWorkCountsAttemptsUntilFailed(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	svc = svc.WithPolicy(lease.Policy{Lease: time.Minute, StuckAfter: time.Hour, MaxAttempts: 2, RetryDelay: time.Minute})
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	w := register(t, svc, "w")

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := svc.Claim(ctx, item.ID, w.ID); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if _, err := svc.Heartbeat(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		age(t, pool, item.ID, "stage_reported_at", 2*time.Hour)
		report, err := svc.Reap(ctx)
		if err != nil || report.Stuck != 1 {
			t.Fatalf("attempt %d: reap %+v, %v; want one stuck", attempt, report, err)
		}
		got, _ := svc.Get(ctx, item.ID)
		if got.Attempts != int32(attempt) {
			t.Fatalf("attempts = %d, want %d", got.Attempts, attempt)
		}
		if attempt == 2 {
			if got.State != work.Failed {
				t.Fatalf("at the limit: %s, want failed", got.State)
			}
			break
		}
		if got.State != work.Ready {
			t.Fatalf("after attempt %d: %s, want ready", attempt, got.State)
		}
		if _, ok := claimStep(t, svc, w.ID, ""); ok {
			t.Fatal("a retried item was handed out before its retry delay")
		}
		age(t, pool, item.ID, "available_at", time.Hour)
	}
	if n := count(eventTypes(t, svc, item.ID), work.EventStuck); n != 2 {
		t.Errorf("%d stuck events, want 2", n)
	}
}

func TestResults(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	svc = svc.WithPolicy(lease.Policy{Lease: time.Minute, StuckAfter: time.Hour, MaxAttempts: 2, RetryDelay: 0})
	w := register(t, svc, "w")
	start := func(title string) db.WorkItem {
		t.Helper()
		item := mustCreate(t, svc, work.NewItem{Title: title})
		if _, err := svc.Claim(ctx, item.ID, w.ID); err != nil {
			t.Fatal(err)
		}
		return item
	}

	released := start("released")
	if got, err := svc.Release(ctx, released.ID, w.ID, "shutting down"); err != nil || got.State != work.Ready || got.Attempts != 0 || got.ClaimedBy != nil {
		t.Errorf("release: %+v, %v; want ready, unclaimed, no attempt", got, err)
	}

	retried := start("retried")
	got, err := svc.ReportResult(ctx, retried.ID, w.ID, work.Result{Outcome: work.Failed, Retryable: true, Reason: "flaky"})
	if err != nil || got.State != work.Ready || got.Attempts != 1 {
		t.Errorf("retryable failure: %s attempts %d, %v; want ready with one attempt", got.State, got.Attempts, err)
	}
	if _, err := svc.Claim(ctx, retried.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	got, err = svc.ReportResult(ctx, retried.ID, w.ID, work.Result{Outcome: work.Failed, Retryable: true})
	if err != nil || got.State != work.Failed || got.Attempts != 2 {
		t.Errorf("retryable failure at the limit: %s attempts %d, %v; want failed", got.State, got.Attempts, err)
	}

	fatal := start("fatal")
	if got, err := svc.ReportResult(ctx, fatal.ID, w.ID, work.Result{Outcome: work.Failed, Reason: "no such repo"}); err != nil || got.State != work.Failed || got.Attempts != 1 {
		t.Errorf("non-retryable failure: %+v, %v", got, err)
	}

	blocked := start("blocked")
	if got, err := svc.ReportResult(ctx, blocked.ID, w.ID, work.Result{Outcome: work.Blocked, Reason: "needs a secret"}); err != nil || got.State != work.Blocked || got.StateReason != "needs a secret" {
		t.Errorf("blocked: %+v, %v", got, err)
	}

	reviewed := start("reviewed")
	if _, err := svc.ReportStage(ctx, reviewed.ID, w.ID, "pr_opened", []work.OutputInput{{Repo: "o/a", Kind: work.KindPullRequest, URL: "u1", Status: work.OutputOpen}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, reviewed.ID); got.Stage != "pr_opened" || got.State != work.InProgress {
		t.Errorf("after stage: %+v", got)
	}
	if got, err := svc.ReportResult(ctx, reviewed.ID, w.ID, work.Result{Outcome: work.InReview}); err != nil || got.State != work.InReview || got.ClaimedBy != nil {
		t.Errorf("in_review: %+v, %v", got, err)
	}

	done := start("done")
	if got, err := svc.ReportResult(ctx, done.ID, w.ID, work.Result{Outcome: work.Completed, Outputs: []work.OutputInput{{Repo: "o/a", Kind: work.KindCommit, SHA: "abc"}}}); err != nil || got.State != work.Completed {
		t.Errorf("completed with a commit: %+v, %v", got, err)
	}

	empty := start("empty")
	if _, err := svc.ReportResult(ctx, empty.ID, w.ID, work.Result{Outcome: work.Completed}); !errors.Is(err, work.ErrNoOutputs) {
		t.Errorf("completed with nothing: %v, want ErrNoOutputs", err)
	}
	if _, err := svc.ReportResult(ctx, empty.ID, w.ID, work.Result{Outcome: "exploded"}); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("unknown outcome: %v, want ErrInvalid", err)
	}
}

func TestSilentWorkerIsMarkedGone(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	w := register(t, svc, "w")
	if _, err := pool.Exec(ctx, `UPDATE workers SET last_seen_at = now() - interval '1 hour' WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Reap(ctx)
	if err != nil || report.Gone != 1 {
		t.Fatalf("reap: %+v, %v; want one worker gone", report, err)
	}
	mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	if _, _, err := svc.ClaimStep(ctx, w.ID, ""); !errors.Is(err, work.ErrWorkerNotActive) {
		t.Fatalf("gone worker claiming: %v, want ErrWorkerNotActive", err)
	}
	if _, err := svc.Heartbeat(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := claimStep(t, svc, w.ID, ""); !ok {
		t.Fatal("a worker that heartbeats again should be able to claim")
	}
}
