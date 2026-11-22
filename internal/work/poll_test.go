package work_test

import (
	"context"
	"encoding/json"
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
