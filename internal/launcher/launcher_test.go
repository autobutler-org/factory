package launcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/launcher"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/repoconfig"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
)

const (
	branch = "b2e9c0de_add-null-check"
	prURL  = "https://github.test/o/a/pull/1"
)

var user = work.Actor{Kind: work.ActorUser, ID: "test"}

type env struct {
	svc *work.Service
	rt  *sandbox.Fake
	l   *launcher.Launcher
}

func newEnv(t *testing.T, maxSandboxes int) env {
	t.Helper()
	pool := dbtest.NewDB(t)
	svc := work.New(pool)
	rt := sandbox.NewFake()
	return env{svc: svc, rt: rt, l: &launcher.Launcher{
		Work: svc, Pool: pool, Runtime: rt, MaxSandboxes: maxSandboxes, Capabilities: []string{"claude"},
		Logger: slog.New(slog.DiscardHandler),
	}}
}

func (e env) create(t *testing.T, repos ...string) db.WorkItem {
	t.Helper()
	res, err := e.svc.Upsert(context.Background(), work.NewItem{Title: "t", Repos: repos}, user)
	if err != nil {
		t.Fatal(err)
	}
	return res.Item
}

func (e env) pass(t *testing.T) launcher.Report {
	t.Helper()
	r, err := e.l.Pass(context.Background())
	if err != nil || !r.Locked {
		t.Fatalf("Pass = %+v, %v", r, err)
	}
	return r
}

func (e env) sandboxes(t *testing.T) []sandbox.Sandbox {
	t.Helper()
	list, err := e.rt.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (e env) step(t *testing.T, itemID, key string) db.WorkStep {
	t.Helper()
	g, err := e.svc.Graph(context.Background(), itemID)
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

func (e env) itemState(t *testing.T, id string) string {
	t.Helper()
	item, err := e.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return item.State
}

// drive plays a sandbox's worker on the chain with sandbox key key, and the
// adapter completing its wait steps with a pass, until step stop is ready.
func (e env) drive(t *testing.T, itemID, key, stop string) {
	t.Helper()
	ctx := context.Background()
	w, err := e.svc.RegisterWorker(ctx, work.WorkerInput{Name: "sandbox-" + key, Kind: work.WorkerCluster})
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if e.step(t, itemID, stop).State == plan.Ready {
			return
		}
		c, ok, err := e.svc.ClaimStep(ctx, w.ID, key)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			g, _ := e.svc.Graph(ctx, itemID)
			for _, s := range g.Steps {
				if plan.IsWait(s.Kind) && s.State == plan.Ready {
					if _, err := e.svc.CompleteWait(ctx, s.ID, work.StepResult{Outcome: work.StepSucceeded, Result: json.RawMessage(`{"verdict":"pass"}`)}, user); err != nil {
						t.Fatal(err)
					}
				}
			}
			continue
		}
		r := work.StepResult{Outcome: work.StepSucceeded, Result: json.RawMessage(`{}`)}
		switch c.Step.Kind {
		case plan.Review:
			r.Result = json.RawMessage(`{"verdict":"pass"}`)
		case plan.OpenPR:
			r.Outputs = []work.OutputInput{{Repo: "o/a", Kind: work.KindPullRequest, Branch: branch, URL: prURL, Status: work.OutputOpen}}
		}
		if _, err := e.svc.ReportStepResult(ctx, c.Step.ID, w.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("runaway graph")
}

func keys(list []sandbox.Sandbox) []string {
	out := make([]string, 0, len(list))
	for _, sb := range list {
		out = append(out, sb.Key)
	}
	slices.Sort(out)
	return out
}

func TestPassPlansAndStartsOneSandboxPerChain(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a", "o/b")

	r := e.pass(t)
	if r.Planned != 1 || len(r.Started) != 2 {
		t.Fatalf("first pass = %+v, want the item planned and both chains started", r)
	}
	if e.itemState(t, item.ID) != work.InProgress {
		t.Errorf("item = %s, want in_progress", e.itemState(t, item.ID))
	}
	want := []string{item.ID + "/o/a", item.ID + "/o/b"}
	if got := keys(e.sandboxes(t)); !slices.Equal(got, want) {
		t.Fatalf("sandboxes for %v, want %v", got, want)
	}

	// Live sandboxes are not started twice.
	if r := e.pass(t); r.Planned != 0 || len(r.Started) != 0 || len(r.Stopped) != 0 {
		t.Errorf("second pass = %+v, want nothing", r)
	}

	// A chain whose sandbox ended with steps still runnable gets a new one.
	first := e.sandboxes(t)[0]
	e.rt.SetPhase(first.Name, sandbox.PhaseFailed)
	r = e.pass(t)
	if len(r.Started) != 1 || r.Started[0] == first.Name {
		t.Fatalf("pass after a sandbox failed = %+v, want one new sandbox", r)
	}
	if sb, _ := e.rt.Status(context.Background(), r.Started[0]); sb.Key != first.Key {
		t.Errorf("new sandbox serves %s, want %s", sb.Key, first.Key)
	}
}

// TestPassStartsEachChainInItsImage starts the chain of a repository that
// names an allowed image in that image, and the other chain in the default.
func TestPassStartsEachChainInItsImage(t *testing.T) {
	e := newEnv(t, 4)
	const image = "ghcr.io/autobutler-org/quark-worker:1.4.0"
	allowed := []string{"ghcr.io/autobutler-org/"}
	e.l.AllowedImages = allowed
	e.l.Work = e.svc.WithConfigs(work.Configs{
		Host: func(_ context.Context, repo string) (codehost.Host, error) {
			if repo == "o/quark" {
				return &codehost.Fake{Files: map[string]string{repoconfig.Path: "gate = \"x\"\nimage = \"" + image + "\"\n"}}, nil
			}
			return &codehost.Fake{Files: map[string]string{repoconfig.Path: "gate = \"x\"\n"}}, nil
		},
		AllowedImages: allowed,
	})
	item := e.create(t, "o/quark", "o/plain")
	if r := e.pass(t); len(r.Started) != 2 {
		t.Fatalf("pass = %+v, want both chains started", r)
	}
	images := map[string]string{}
	for _, sb := range e.sandboxes(t) {
		images[sb.Key] = sb.Image
	}
	if images[item.ID+"/o/quark"] != image || images[item.ID+"/o/plain"] != "" {
		t.Errorf("sandbox images = %v; want %s for o/quark and the default for o/plain", images, image)
	}
}

// TestPassBlocksAChainWhoseImageIsNoLongerAllowed checks the image again at
// launch: an allowlist narrowed after the item was planned blocks it rather
// than run the image or fall back to the default.
func TestPassBlocksAChainWhoseImageIsNoLongerAllowed(t *testing.T) {
	e := newEnv(t, 4)
	e.l.Work = e.svc.WithConfigs(work.Configs{
		Host: func(context.Context, string) (codehost.Host, error) {
			return &codehost.Fake{Files: map[string]string{repoconfig.Path: "gate = \"x\"\nimage = \"ghcr.io/autobutler-org/x:1\"\n"}}, nil
		},
		AllowedImages: []string{"ghcr.io/autobutler-org/"},
	})
	item := e.create(t, "o/a")
	r := e.pass(t)
	if r.Planned != 1 || len(r.Started) != 0 || len(e.sandboxes(t)) != 0 {
		t.Fatalf("pass = %+v, sandboxes %v; want the item planned and no sandbox", r, e.sandboxes(t))
	}
	got, err := e.svc.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != work.Blocked || !strings.Contains(got.StateReason, "o/a") || !strings.Contains(got.StateReason, "not allowed") {
		t.Errorf("item %s (%q), want blocked because o/a's image is not allowed", got.State, got.StateReason)
	}
}

func TestPassKeepsUnderTheCap(t *testing.T) {
	e := newEnv(t, 1)
	e.create(t, "o/a", "o/b")
	e.create(t, "o/c")
	r := e.pass(t)
	if r.Planned != 1 || len(r.Started) != 1 || len(e.sandboxes(t)) != 1 {
		t.Fatalf("pass = %+v, sandboxes %v; want one item planned and one sandbox", r, e.sandboxes(t))
	}
	if r := e.pass(t); r.Planned != 0 || len(r.Started) != 0 {
		t.Errorf("pass at the cap = %+v, want nothing", r)
	}
}

func TestPassStopsOnlyChainsWithNothingLeft(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	key := item.ID + "/o/a"
	sb := e.sandboxes(t)[0]
	e.rt.SetPhase(sb.Name, sandbox.PhaseRunning)

	// The sandbox's worker holds a step: the chain is not runnable, but the
	// sandbox stays.
	w, err := e.svc.RegisterWorker(ctx, work.WorkerInput{Name: sb.Name, Kind: work.WorkerCluster})
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := e.svc.ClaimStep(ctx, w.ID, key)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if r := e.pass(t); len(r.Stopped) != 0 || len(r.Started) != 0 {
		t.Fatalf("pass while the worker holds %s = %+v, want nothing stopped or started", c.Step.Key, r)
	}

	// The chain waits on checks: nothing runnable, steps still to come.
	if _, err := e.svc.ReportStepResult(ctx, c.Step.ID, w.ID, work.StepResult{Outcome: work.StepSucceeded}); err != nil {
		t.Fatal(err)
	}
	e.drive(t, item.ID, key, "o/a/await_checks-1")
	if r := e.pass(t); len(r.Stopped) != 0 || len(r.Started) != 0 {
		t.Fatalf("pass while the chain waits = %+v, want nothing", r)
	}

	// The item is blocked: its steps are canceled and the sandbox goes.
	if _, err := e.svc.Transition(ctx, item.ID, work.Blocked, "needs a decision", user); err != nil {
		t.Fatal(err)
	}
	if r := e.pass(t); !slices.Equal(r.Stopped, []string{sb.Name}) {
		t.Fatalf("pass after the item was blocked = %+v, want %s stopped", r, sb.Name)
	}
	if len(e.sandboxes(t)) != 0 {
		t.Errorf("sandboxes left: %v", e.sandboxes(t))
	}
}

func TestPassReplacesASandboxStuckPending(t *testing.T) {
	e := newEnv(t, 4)
	e.l.PendingTimeout = 10 * time.Minute
	item := e.create(t, "o/a")
	stuck := e.pass(t).Started[0]

	// Pending for less than the timeout: left alone.
	e.rt.SetCreated(stuck, time.Now().Add(-5*time.Minute))
	if r := e.pass(t); len(r.Stopped)+len(r.Started) != 0 {
		t.Fatalf("pass before the timeout = %+v, want nothing", r)
	}
	// Pending for longer, say on an image pull error: destroyed, and the chain
	// gets a new sandbox in the same pass.
	e.rt.SetCreated(stuck, time.Now().Add(-11*time.Minute))
	r := e.pass(t)
	if !slices.Equal(r.Stopped, []string{stuck}) || len(r.Started) != 1 {
		t.Fatalf("pass after the timeout = %+v, want %s stopped and one started", r, stuck)
	}
	if got := keys(e.sandboxes(t)); !slices.Equal(got, []string{item.ID + "/o/a"}) {
		t.Errorf("sandboxes for %v", got)
	}
	// A running sandbox is never timed out.
	e.rt.SetPhase(r.Started[0], sandbox.PhaseRunning)
	e.rt.SetCreated(r.Started[0], time.Now().Add(-time.Hour))
	if r := e.pass(t); len(r.Stopped) != 0 {
		t.Errorf("pass with an old running sandbox = %+v, want nothing stopped", r)
	}
}

func TestPassWaitsForTheOtherReplica(t *testing.T) {
	e := newEnv(t, 4)
	e.create(t, "o/a")
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := db.TryLock(context.Background(), e.l.Pool, 0x666163746f7279, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
		done <- err
	}()
	<-held
	r, err := e.l.Pass(context.Background())
	close(release)
	if err != nil || r.Locked || r.Planned != 0 || len(e.sandboxes(t)) != 0 {
		t.Fatalf("Pass while another replica holds the lock = %+v, %v; want nothing done", r, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func openPull() *codehost.Fake {
	f := &codehost.Fake{Repo: "o/a"}
	if _, err := f.OpenPull(context.Background(), "t", "", branch, "main"); err != nil {
		panic(err)
	}
	return f
}

func TestServerMerge(t *testing.T) {
	tests := []struct {
		name       string
		host       func() *codehost.Fake
		wantResult string
		wantItem   string
		wantMerged []int
	}{
		{"merges the pull request", openPull, `{"merged": true, "queued": false}`, work.Completed, []int{1}},
		{"enqueues when the base has a merge queue", func() *codehost.Fake {
			f := openPull()
			f.QueueID = "PR_node1"
			return f
		}, `{"merged": false, "queued": true}`, work.InReview, nil},
		{"a pull request already merged is not merged again", func() *codehost.Fake {
			f := openPull()
			f.Pulls[0].State, f.Pulls[0].MergedAt = "closed", "2026-11-18T12:00:00Z"
			return f
		}, `{"merged": true, "queued": false}`, work.Completed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, 4)
			item := e.create(t, "o/a")
			e.pass(t)
			e.drive(t, item.ID, item.ID+"/o/a", "o/a/merge")

			// No GitHub App: the merge step stays ready.
			if r := e.pass(t); r.ServerSteps != 0 || e.step(t, item.ID, "o/a/merge").State != plan.Ready {
				t.Fatalf("pass without a code host = %+v, merge %s", r, e.step(t, item.ID, "o/a/merge").State)
			}

			host := tt.host()
			var asked []string
			e.l.Host = func(_ context.Context, repo string) (codehost.Host, error) {
				asked = append(asked, repo)
				return host, nil
			}
			if r := e.pass(t); r.ServerSteps != 1 {
				t.Fatalf("pass = %+v, want one server step", r)
			}
			merge := e.step(t, item.ID, "o/a/merge")
			if merge.State != plan.Succeeded || string(merge.Result) != tt.wantResult {
				t.Fatalf("merge = %s %s, want succeeded %s", merge.State, merge.Result, tt.wantResult)
			}
			// The poll asks for the same repository's host.
			if !slices.Equal(slices.Compact(asked), []string{"o/a"}) || !slices.Equal(host.Merged(), tt.wantMerged) {
				t.Errorf("host asked for %v, merged %v; want only o/a and %v", asked, host.Merged(), tt.wantMerged)
			}
			if got := e.itemState(t, item.ID); got != tt.wantItem {
				t.Errorf("item = %s, want %s", got, tt.wantItem)
			}
			if r := e.pass(t); r.ServerSteps != 0 {
				t.Errorf("a later pass ran %d server steps, want 0", r.ServerSteps)
			}
		})
	}
}

func TestServerMergeFailures(t *testing.T) {
	e := newEnv(t, 4)
	item := e.create(t, "o/a")
	e.pass(t)
	e.drive(t, item.ID, item.ID+"/o/a", "o/a/merge")
	e.l.Host = func(context.Context, string) (codehost.Host, error) { return nil, errors.New("github is down") }

	e.pass(t)
	merge := e.step(t, item.ID, "o/a/merge")
	if merge.State != plan.Ready || merge.Attempts != 1 {
		t.Fatalf("merge after a code host error = %s with %d attempts, want ready again with 1", merge.State, merge.Attempts)
	}
	events, err := e.svc.Events(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	var payload struct {
		Outcome   string `json:"outcome"`
		Retryable bool   `json:"retryable"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(last.Payload, &payload)
	if last.Type != work.EventStepResult || payload.Outcome != work.StepFailed || !payload.Retryable || payload.Reason != "code host for o/a: github is down" {
		t.Errorf("last event = %s %s", last.Type, last.Payload)
	}
}
