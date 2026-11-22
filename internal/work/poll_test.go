package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

// TestPollPullsListsOpenPullsWithTheirWait lists every open pull request of
// an item being worked, the ready wait step of its repository beside the
// newest one, and since when it has waited.
func TestPollPullsListsOpenPullsWithTheirWait(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	runUntil(t, svc, w.ID, item.ID, "o/a/review-1")
	planned := step(t, svc, item.ID, "o/a/workspace").AvailableAt

	rows, err := svc.PollPulls(ctx)
	if err != nil || len(rows) != 1 || rows[0].StepID != nil || rows[0].Url != "https://codehost.test/o/a/pull/1" {
		t.Fatalf("PollPulls during review = %+v, %v; want the pull request and no wait", rows, err)
	}
	runUntil(t, svc, w.ID, item.ID, "o/a/await_checks-1")
	// An older pull request of the same repository is still polled, but the
	// wait belongs to the newest.
	if _, err := svc.RecordOutput(ctx, item.ID, work.OutputInput{Repo: "o/a", Kind: work.KindPullRequest, URL: "https://codehost.test/o/a/pull/2", Status: work.OutputOpen}, adapter); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.PollPulls(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("PollPulls = %+v, %v; want both pull requests", rows, err)
	}
	waits := map[string]db.PollPullsRow{}
	for _, r := range rows {
		waits[r.Url] = r
	}
	older, newer := waits["https://codehost.test/o/a/pull/1"], waits["https://codehost.test/o/a/pull/2"]
	checks := step(t, svc, item.ID, "o/a/await_checks-1")
	if older.StepID != nil || newer.StepID == nil || *newer.StepID != checks.ID || *newer.StepKind != plan.AwaitChecks || *newer.StepKey != "o/a/await_checks-1" {
		t.Fatalf("rows = %+v; want await_checks-1 beside the newest pull request only", rows)
	}
	if newer.ReadySince == nil || !newer.ReadySince.After(planned) {
		t.Errorf("ready_since = %v, want when the step became ready, after planning at %v", newer.ReadySince, planned)
	}

	// The completion's event carries what the completer observed.
	observed := `{"head": "abc", "verdict": "pass"}` // as jsonb prints it
	if _, err := svc.CompleteWait(ctx, checks.ID, succeed(observed), adapter); err != nil {
		t.Fatal(err)
	}
	events, err := svc.Events(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		var p struct {
			Key    string          `json:"key"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if e.Type == work.EventStepCompleted && p.Key == "o/a/await_checks-1" {
			found = e.ActorID == adapter.ID && string(p.Result) == observed
		}
	}
	if !found {
		t.Errorf("no step_completed event by %s with result %s", adapter.ID, observed)
	}

	// An item no longer being worked is not polled.
	if _, err := svc.Transition(ctx, item.ID, work.Blocked, "stop", adapter); err != nil {
		t.Fatal(err)
	}
	if rows, err := svc.PollPulls(ctx); err != nil || len(rows) != 0 {
		t.Errorf("PollPulls of a blocked item = %+v, %v; want none", rows, err)
	}
}

// TestObservePull records what the code host says became of a pull request:
// merged completes the item once nothing is open, closed without merging
// blocks it, and seeing the same thing again changes nothing.
func TestObservePull(t *testing.T) {
	ctx := context.Background()
	const a, b = "https://codehost.test/o/a/pull/1", "https://codehost.test/o/b/pull/1"
	pull := func(repo, url, status string) work.OutputInput {
		return work.OutputInput{Repo: repo, Kind: work.KindPullRequest, URL: url, SHA: "abc", Status: status}
	}
	start := func(t *testing.T) (*work.Service, db.WorkItem) {
		t.Helper()
		svc, _ := newService(t)
		w := register(t, svc, "w")
		item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a", "o/b"}})
		runUntil(t, svc, w.ID, item.ID, "")
		if got := itemState(t, svc, item.ID); got != work.InReview {
			t.Fatalf("item = %s, want in_review with both pull requests open", got)
		}
		return svc, item
	}

	t.Run("the item completes when every pull request is merged", func(t *testing.T) {
		svc, item := start(t)
		if changed, err := svc.ObservePull(ctx, item.ID, pull("o/a", a, work.OutputMerged), adapter); err != nil || !changed {
			t.Fatalf("ObservePull merged = %v, %v", changed, err)
		}
		if got := itemState(t, svc, item.ID); got != work.InReview {
			t.Fatalf("item with o/b still open = %s, want in_review", got)
		}
		before, _ := svc.Events(ctx, item.ID)
		if changed, err := svc.ObservePull(ctx, item.ID, pull("o/a", a, work.OutputMerged), adapter); err != nil || changed {
			t.Fatalf("ObservePull merged again = %v, %v; want no change", changed, err)
		}
		if after, _ := svc.Events(ctx, item.ID); len(after) != len(before) {
			t.Errorf("seeing the merge again wrote %d events", len(after)-len(before))
		}
		if _, err := svc.ObservePull(ctx, item.ID, pull("o/b", b, work.OutputMerged), adapter); err != nil {
			t.Fatal(err)
		}
		if got := itemState(t, svc, item.ID); got != work.Completed {
			t.Errorf("item = %s, want completed", got)
		}
		for key, st := range states(t, svc, item.ID) {
			if st == plan.Ready || st == plan.Pending {
				t.Errorf("step %s = %s after completion, want canceled", key, st)
			}
		}
	})

	t.Run("a pull request closed without merging blocks the item", func(t *testing.T) {
		svc, item := start(t)
		if _, err := svc.ObservePull(ctx, item.ID, pull("o/a", a, work.OutputMerged), adapter); err != nil {
			t.Fatal(err)
		}
		if changed, err := svc.ObservePull(ctx, item.ID, pull("o/b", b, work.OutputClosed), adapter); err != nil || !changed {
			t.Fatalf("ObservePull closed = %v, %v", changed, err)
		}
		got, err := svc.Get(ctx, item.ID)
		if err != nil || got.State != work.Blocked || got.StateReason != "pull request "+b+" was closed without merging" {
			t.Fatalf("item = %s %q, %v; want blocked with the pull request named, not completed", got.State, got.StateReason, err)
		}
		outputs, _ := svc.Outputs(ctx, item.ID)
		for _, o := range outputs {
			if o.Url == b && o.Status != work.OutputClosed {
				t.Errorf("output %s = %s, want closed", o.Url, o.Status)
			}
		}
	})

	t.Run("an unknown pull request", func(t *testing.T) {
		svc, item := start(t)
		if _, err := svc.ObservePull(ctx, item.ID, pull("o/a", "https://codehost.test/o/a/pull/9", work.OutputMerged), adapter); !errors.Is(err, work.ErrNotFound) {
			t.Errorf("ObservePull of a pull request not recorded = %v, want ErrNotFound", err)
		}
	})
}
