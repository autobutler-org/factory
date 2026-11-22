package launcher_test

import (
	"context"
	"encoding/json"
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
