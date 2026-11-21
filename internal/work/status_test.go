package work_test

import (
	"context"
	"testing"

	"github.com/autobutler-org/factory/internal/work"
)

func TestStatus(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	first := mustCreate(t, svc, work.NewItem{Title: "first ready", IdempotencyKey: "a"})
	mustCreate(t, svc, work.NewItem{Title: "second ready", IdempotencyKey: "b"})
	mustCreate(t, svc, work.NewItem{Title: "an idea", State: work.Design, IdempotencyKey: "c"})
	for i, key := range []string{"f1", "f2", "f3", "f4", "f5", "f6"} {
		item := mustCreate(t, svc, work.NewItem{Title: key, IdempotencyKey: key})
		if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'failed', state_changed_at = now() + make_interval(secs => $2) WHERE id = $1`, item.ID, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.RegisterWorker(ctx, work.WorkerInput{Name: "w", Kind: work.WorkerCluster}); err != nil {
		t.Fatal(err)
	}

	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Counts[work.Ready] != 2 || st.Counts[work.Design] != 1 || st.Counts[work.Failed] != 6 || st.Counts[work.Completed] != 0 {
		t.Errorf("counts = %v, want 2 ready, 1 design, 6 failed and a zero for every other state", st.Counts)
	}
	if len(st.Counts) != len(work.States()) {
		t.Errorf("counts has %d states, want all %d", len(st.Counts), len(work.States()))
	}
	if st.OldestReady == nil || st.OldestReady.ID != first.ID {
		t.Errorf("oldest ready = %+v, want %q", st.OldestReady, first.Title)
	}
	if len(st.RecentFailures) != 5 || st.RecentFailures[0].Title != "f6" || st.RecentFailures[4].Title != "f2" {
		t.Errorf("recent failures = %d, want the five latest, newest first", len(st.RecentFailures))
	}
	if st.Workers[work.WorkerActive] != 1 {
		t.Errorf("workers = %v, want 1 active", st.Workers)
	}
}
