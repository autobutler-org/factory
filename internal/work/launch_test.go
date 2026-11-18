package work_test

import (
	"context"
	"slices"
	"testing"

	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

var launched = []string{"claude"}

func runnable(t *testing.T, svc *work.Service) []string {
	t.Helper()
	keys, err := svc.RunnableSandboxes(context.Background(), launched)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestStartReadyPlansUpToTheLimit(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	low := mustCreate(t, svc, work.NewItem{Title: "low", Repos: []string{"o/a"}})
	high := mustCreate(t, svc, work.NewItem{Title: "high", Repos: []string{"o/a"}, Priority: 5})
	gpu := mustCreate(t, svc, work.NewItem{Title: "gpu", Repos: []string{"o/a"}, Priority: 9, Requires: []string{"gpu"}})
	claude := mustCreate(t, svc, work.NewItem{Title: "claude", Repos: []string{"o/a"}, Requires: []string{"claude"}})

	n, err := svc.StartReady(ctx, launched, 1)
	if err != nil || n != 1 {
		t.Fatalf("StartReady(1) = %d, %v; want 1", n, err)
	}
	if itemState(t, svc, high.ID) != work.InProgress || itemState(t, svc, low.ID) != work.Ready {
		t.Fatal("the highest-priority item the workers can run was not planned first")
	}
	if states(t, svc, high.ID)["o/a/workspace"] != plan.Ready {
		t.Errorf("planned graph: %v", states(t, svc, high.ID))
	}
	types := eventTypes(t, svc, high.ID)
	if count(types, work.EventPlanned) != 1 {
		t.Errorf("events: %v", types)
	}

	n, err = svc.StartReady(ctx, launched, 10)
	if err != nil || n != 2 {
		t.Fatalf("StartReady(10) = %d, %v; want the two remaining items the workers can run", n, err)
	}
	if itemState(t, svc, gpu.ID) != work.Ready || itemState(t, svc, claude.ID) != work.InProgress {
		t.Error("an item was planned for workers that lack its requirements, or one they meet was not")
	}
	if n, err := svc.StartReady(ctx, launched, 0); err != nil || n != 0 {
		t.Errorf("StartReady(0) = %d, %v", n, err)
	}
}

func TestRunnableSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	two := mustCreate(t, svc, work.NewItem{Title: "two", Repos: []string{"o/a", "o/b"}, Priority: 1})
	gpu := mustCreate(t, svc, work.NewItem{Title: "gpu", Repos: []string{"o/a"}, Requires: []string{"gpu"}})
	if got := runnable(t, svc); len(got) != 0 {
		t.Fatalf("runnable before planning: %v", got)
	}
	if _, err := svc.StartReady(ctx, []string{"claude", "gpu"}, 10); err != nil {
		t.Fatal(err)
	}
	keyA, keyB := two.ID+"/o/a", two.ID+"/o/b"
	if got := runnable(t, svc); !slices.Equal(got, []string{keyA, keyB}) {
		t.Fatalf("runnable = %v, want both chains of the item the workers can run, and not %s", got, gpu.ID)
	}

	// A chain with a running step has its sandbox already.
	c, ok := claimStep(t, svc, w.ID, keyA)
	if !ok {
		t.Fatal("restricted claim got nothing")
	}
	if got := runnable(t, svc); !slices.Equal(got, []string{keyB}) {
		t.Fatalf("runnable with chain a running = %v", got)
	}
	report(t, svc, c, w.ID, fakeResult(c))
	if got := runnable(t, svc); !slices.Equal(got, []string{keyA, keyB}) {
		t.Fatalf("runnable after the step = %v", got)
	}

	// A chain waiting on an event needs no sandbox.
	for states(t, svc, two.ID)["o/a/await_checks-1"] != plan.Ready {
		next, ok := claimStep(t, svc, w.ID, keyA)
		if !ok {
			t.Fatalf("chain a stalled: %v", states(t, svc, two.ID))
		}
		report(t, svc, next, w.ID, fakeResult(next))
	}
	if got := runnable(t, svc); !slices.Equal(got, []string{keyB}) {
		t.Fatalf("runnable with chain a waiting = %v", got)
	}
}

func TestEndIdleSandbox(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	if _, err := svc.StartReady(ctx, launched, 1); err != nil {
		t.Fatal(err)
	}
	key := item.ID + "/o/a"
	calls := 0
	end := func(context.Context) error { calls++; return nil }

	ended, err := svc.EndIdleSandbox(ctx, key, end)
	if err != nil || ended || calls != 0 {
		t.Fatalf("a chain with steps left was ended: %v %v %d", ended, err, calls)
	}

	// Blocking the item cancels its steps; the chain is done.
	if _, err := svc.Transition(ctx, item.ID, work.Blocked, "needs a decision", user); err != nil {
		t.Fatal(err)
	}
	ended, err = svc.EndIdleSandbox(ctx, key, end)
	if err != nil || !ended || calls != 1 {
		t.Fatalf("a finished chain was not ended: %v %v %d", ended, err, calls)
	}

	// A key that names no item is not ours to keep.
	ended, err = svc.EndIdleSandbox(ctx, "not-an-item/o/a", end)
	if err != nil || !ended || calls != 2 {
		t.Fatalf("an unknown key was not ended: %v %v %d", ended, err, calls)
	}
}

func TestClaimServerStep(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w", "gpu")
	server := register(t, svc, "factory-server")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}, Requires: []string{"gpu"}})
	key := item.ID + "/o/a"

	if _, ok, err := svc.ClaimServerStep(ctx, server.ID); err != nil || ok {
		t.Fatalf("server claimed before planning: %v %v", ok, err)
	}
	if _, err := svc.StartReady(ctx, []string{"gpu"}, 1); err != nil {
		t.Fatal(err)
	}
	for states(t, svc, item.ID)["o/a/merge"] != plan.Ready {
		c, ok := claimStep(t, svc, w.ID, key)
		if !ok {
			completeReadyWaits(t, svc, item.ID)
			continue
		}
		if _, ok, _ := svc.ClaimServerStep(ctx, server.ID); ok {
			t.Fatalf("server claimed a sandbox step while %s ran", c.Step.Key)
		}
		report(t, svc, c, w.ID, fakeResult(c))
	}
	if _, ok := claimStep(t, svc, w.ID, key); ok {
		t.Fatal("a sandboxed worker claimed merge")
	}

	// The server needs none of the item's requirements to merge.
	c, ok, err := svc.ClaimServerStep(ctx, server.ID)
	if err != nil || !ok || c.Step.Kind != plan.Merge || len(c.Outputs) != 1 {
		t.Fatalf("server claim = %+v %v %v", c.Step, ok, err)
	}
	report(t, svc, c, server.ID, fakeResult(c))
	if got := itemState(t, svc, item.ID); got != work.Completed {
		t.Errorf("item after merge = %s, want completed", got)
	}
	types := eventTypes(t, svc, item.ID)
	if count(types, work.EventStepClaimed) == 0 || count(types, work.EventStepResult) == 0 {
		t.Errorf("events: %v", types)
	}
}
