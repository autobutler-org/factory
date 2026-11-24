package launcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/repoconfig"
	"github.com/autobutler-org/factory/internal/work"
)

// polled points the launcher at host, polling on every pass.
func (e env) polled(host *codehost.Fake) {
	e.l.Host = func(context.Context, string) (codehost.Host, error) { return host, nil }
	e.l.PollInterval = 0
}

// lastEvent returns the newest event of type typ for step key.
func (e env) lastEvent(t *testing.T, itemID, typ, key string) (actor string, result json.RawMessage) {
	t.Helper()
	events, err := e.svc.Events(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		var p struct {
			Key    string          `json:"key"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if ev.Type == typ && p.Key == key {
			actor, result = ev.ActorKind+"/"+ev.ActorID, p.Result
		}
	}
	return actor, result
}

// TestPollCompletesChecks runs one chain's checks through a failure, the fix
// push that changes the head, and a pass, with nothing but launcher passes
// completing the waits; the merge then runs in the same pass.
func TestPollCompletesChecks(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	host := openPull()
	host.Pulls[0].Head.Sha = "head-1"
	host.HeadChecks = map[string][]codehost.Check{"head-1": {{Name: "ci", State: codehost.CheckFailed}, {Name: "lint", State: codehost.CheckPassed}}}
	e.polled(host)

	if r := e.pass(t); r.Polled != 1 {
		t.Fatalf("pass = %+v, want one wait completed", r)
	}
	checks := e.step(t, item.ID, "o/a/await_checks-1")
	var got struct {
		Verdict      string   `json:"verdict"`
		FailedChecks []string `json:"failed_checks"`
		Head         string   `json:"head"`
	}
	if err := json.Unmarshal(checks.Result, &got); err != nil || checks.State != plan.Succeeded || got.Verdict != "fail" ||
		len(got.FailedChecks) != 1 || got.FailedChecks[0] != "ci" || got.Head != "head-1" {
		t.Fatalf("await_checks-1 = %s %s, want succeeded with fail, ci, head-1", checks.State, checks.Result)
	}
	if s := e.step(t, item.ID, "o/a/fix-checks-1").State; s != plan.Ready {
		t.Fatalf("fix-checks-1 = %s, want ready", s)
	}
	if actor, result := e.lastEvent(t, item.ID, work.EventStepCompleted, "o/a/await_checks-1"); actor != "system/poller" || len(result) == 0 {
		t.Errorf("step_completed by %s with %s; want system/poller and what it saw", actor, result)
	}

	// The fix pushes a new head. Its checks are still running, and the old
	// head's failure no longer counts.
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-2")
	host.Pulls[0].Head.Sha = "head-2"
	host.HeadChecks["head-2"] = []codehost.Check{{Name: "ci", State: codehost.CheckPending}}
	if r := e.pass(t); r.Polled != 0 || e.step(t, item.ID, "o/a/await_checks-2").State != plan.Ready {
		t.Fatalf("pass with checks running = %+v; want await_checks-2 still waiting", r)
	}
	host.HeadChecks["head-2"] = []codehost.Check{{Name: "ci", State: codehost.CheckPassed}}
	if r := e.pass(t); r.Polled != 1 || r.ServerSteps != 1 {
		t.Fatalf("pass with checks green = %+v, want the wait completed and the merge run", r)
	}
	if got := e.itemState(t, item.ID); got != work.Completed {
		t.Errorf("item = %s, want completed", got)
	}
}

// TestPollRequiredChecks counts only the required checks when the base
// branch names some, and waits for a required check that has not reported.
func TestPollRequiredChecks(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	host := openPull()
	host.Required = []string{"ci", "deploy"}
	host.HeadChecks = map[string][]codehost.Check{"": {{Name: "ci", State: codehost.CheckPassed}, {Name: "flaky", State: codehost.CheckFailed}}}
	e.polled(host)
	if r := e.pass(t); r.Polled != 0 {
		t.Fatalf("pass with the required deploy unreported = %+v, want nothing", r)
	}
	host.HeadChecks[""] = append(host.HeadChecks[""], codehost.Check{Name: "deploy", State: codehost.CheckPassed})
	e.pass(t)
	if s := e.step(t, item.ID, "o/a/await_checks-1"); plan.Verdict(s.Result) != "pass" {
		t.Errorf("await_checks-1 = %s %s, want pass: the failed check is not required", s.State, s.Result)
	}
}

// TestPollRepositoryWithoutChecks passes a head with no checks once the
// grace period has gone by, and not before.
func TestPollRepositoryWithoutChecks(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	e.polled(openPull())
	e.l.ChecksGrace = time.Hour
	if r := e.pass(t); r.Polled != 0 {
		t.Fatalf("pass inside the grace period = %+v, want nothing", r)
	}
	e.l.ChecksGrace = 0
	e.pass(t)
	if s := e.step(t, item.ID, "o/a/await_checks-1"); s.State != plan.Succeeded || plan.Verdict(s.Result) != "pass" {
		t.Errorf("await_checks-1 = %s %s, want a pass with no checks", s.State, s.Result)
	}
}

// TestPollCompletesApproval completes await_approval once the pull request
// is approved, for a repository that requires approval and merges by hand.
func TestPollCompletesApproval(t *testing.T) {
	e := newEnv(t, 4)
	host := openPull()
	host.Files = map[string]string{repoconfig.Path: "gate = \"x\"\nrequire_approval = true\n[automate]\nmerge = \"off\"\n"}
	host.HeadChecks = map[string][]codehost.Check{"": {{Name: "ci", State: codehost.CheckPassed}}}
	e.l.Work = e.svc.WithConfigs(work.Configs{Host: func(context.Context, string) (codehost.Host, error) { return host, nil }})
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	e.polled(host)
	e.pass(t)
	if s := e.step(t, item.ID, "o/a/await_approval").State; s != plan.Ready {
		t.Fatalf("await_approval = %s, want ready once checks pass", s)
	}
	if r := e.pass(t); r.Polled != 0 {
		t.Fatalf("pass before an approval = %+v, want nothing", r)
	}
	host.Approved = []int{1}
	e.pass(t)
	if s := e.step(t, item.ID, "o/a/await_approval").State; s != plan.Succeeded {
		t.Errorf("await_approval = %s, want succeeded", s)
	}
	// Merge is off: the item waits for a person to merge.
	if got := e.itemState(t, item.ID); got != work.InReview {
		t.Errorf("item = %s, want in_review", got)
	}
}

// TestPollWaitsForItsInterval polls on the first pass and then not until
// PollInterval has gone by, however often the launcher passes.
func TestPollWaitsForItsInterval(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	host := openPull()
	host.HeadChecks = map[string][]codehost.Check{"": {{Name: "ci", State: codehost.CheckPending}}}
	e.polled(host)
	e.l.PollInterval = time.Hour
	e.pass(t)
	host.HeadChecks[""] = []codehost.Check{{Name: "ci", State: codehost.CheckPassed}}
	if r := e.pass(t); r.Polled != 0 || e.step(t, item.ID, "o/a/await_checks-1").State != plan.Ready {
		t.Errorf("a pass before the interval = %+v; want no poll", r)
	}
}

// TestPollMergedByAPerson completes an item whose pull request a person
// merged while its chain still waited on checks: the output is recorded
// merged by the poller with the head it saw, and the steps left are canceled.
func TestPollMergedByAPerson(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	host := openPull()
	host.Pulls[0].Head.Sha = "head-1"
	host.HeadChecks = map[string][]codehost.Check{"head-1": {{Name: "ci", State: codehost.CheckPending}}}
	e.polled(host)
	e.pass(t)
	if err := host.MergePull(context.Background(), 1); err != nil { // by a person
		t.Fatal(err)
	}
	if r := e.pass(t); r.Polled != 1 || r.ServerSteps != 0 {
		t.Fatalf("pass after the merge = %+v, want the merge recorded and no merge step run", r)
	}
	if got := e.itemState(t, item.ID); got != work.Completed {
		t.Fatalf("item = %s, want completed", got)
	}
	for _, key := range []string{"o/a/await_checks-1", "o/a/merge"} {
		if s := e.step(t, item.ID, key).State; s != plan.Canceled {
			t.Errorf("%s = %s, want canceled", key, s)
		}
	}
	outputs, err := e.svc.Outputs(context.Background(), item.ID)
	if err != nil || len(outputs) != 1 || outputs[0].Status != work.OutputMerged || outputs[0].Sha != "head-1" {
		t.Errorf("outputs = %+v, %v; want the pull request merged at head-1", outputs, err)
	}
	events, _ := e.svc.Events(context.Background(), item.ID)
	last := events[len(events)-1]
	if last.ActorKind+"/"+last.ActorID != "system/poller" || last.ToState == nil || *last.ToState != work.Completed {
		t.Errorf("last event = %s by %s/%s to %v, want the poller completing the item", last.Type, last.ActorKind, last.ActorID, last.ToState)
	}
	if r := e.pass(t); r.Polled != 0 {
		t.Errorf("a later pass = %+v; a completed item is not polled", r)
	}
}

// TestPollMergedAfterTheItemFailed completes a failed item when a person
// fixes and merges its pull request: the poller keeps watching an open pull
// request after the item fails, and stops once it has merged.
func TestPollMergedAfterTheItemFailed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	if _, err := e.svc.Transition(ctx, item.ID, work.Failed, "the checks failed on the last round", work.Actor{Kind: work.ActorSystem, ID: "test"}); err != nil {
		t.Fatal(err)
	}
	host := openPull()
	e.polled(host)
	e.l.PollInterval = 0
	if r := e.pass(t); r.Polled != 0 || e.itemState(t, item.ID) != work.Failed {
		t.Fatalf("pass with the pull request open = %+v; want the failed item left alone", r)
	}
	if err := host.MergePull(ctx, 1); err != nil { // by a person
		t.Fatal(err)
	}
	if r := e.pass(t); r.Polled != 1 || e.itemState(t, item.ID) != work.Completed {
		t.Fatalf("pass after the merge = %+v, item %s; want it completed", r, e.itemState(t, item.ID))
	}
	if r := e.pass(t); r.Polled != 0 {
		t.Errorf("a later pass = %+v; a merged pull request is not polled", r)
	}
}

// TestPollClosedWithoutMerge blocks the item, naming the pull request, so a
// person decides whether to reopen it, start over or drop the item.
func TestPollClosedWithoutMerge(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/review-1")
	host := openPull()
	host.Pulls[0].State = "closed"
	e.polled(host)
	e.pass(t)
	got, err := e.svc.Get(context.Background(), item.ID)
	if err != nil || got.State != work.Blocked || got.StateReason != "pull request https://github.test/o/a/pull/1 was closed without merging" {
		t.Fatalf("item = %s %q, %v; want blocked with the reason", got.State, got.StateReason, err)
	}
	if s := e.step(t, item.ID, "o/a/review-1").State; s != plan.Canceled {
		t.Errorf("review-1 = %s, want canceled", s)
	}
}

// TestPollSeveralRepositories completes an item once the pull requests of
// all its repositories are merged, and polls the others while one repository
// cannot be read.
func TestPollSeveralRepositories(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a", "o/b")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/await_checks-1")
	e.drive(t, item.ID, item.ID+"/o/b", "o/b/await_checks-1")
	hosts := map[string]*codehost.Fake{"o/a": openPull(), "o/b": {Repo: "o/b"}}
	if _, err := hosts["o/b"].OpenPull(context.Background(), "t", "", branch, "main"); err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		h.HeadChecks = map[string][]codehost.Check{"": {{Name: "ci", State: codehost.CheckPending}}}
	}
	e.l.Host = func(_ context.Context, repo string) (codehost.Host, error) { return hosts[repo], nil }
	e.l.PollInterval = 0

	hosts["o/a"].StatusErr = errors.New("rate limited")
	hosts["o/b"].HeadChecks[""] = []codehost.Check{{Name: "ci", State: codehost.CheckPassed}}
	if r := e.pass(t); r.Polled != 1 || e.step(t, item.ID, "o/b/await_checks-1").State != plan.Succeeded {
		t.Fatalf("pass with o/a unreadable = %+v; want o/b's checks completed", r)
	}
	if got := e.itemState(t, item.ID); got != work.InReview {
		t.Fatalf("item with o/b merged and o/a open = %s, want in_review", got)
	}
	hosts["o/a"].StatusErr = nil
	if err := hosts["o/a"].MergePull(context.Background(), 1); err != nil { // by a person
		t.Fatal(err)
	}
	e.pass(t)
	if got := e.itemState(t, item.ID); got != work.Completed {
		t.Errorf("item = %s, want completed with both merged", got)
	}
}
