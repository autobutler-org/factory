package work_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	user   = work.Actor{Kind: work.ActorUser, ID: "tester"}
	worker = work.Actor{Kind: work.ActorWorker, ID: "w1"}
)

func newService(t *testing.T) (*work.Service, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewDB(t)
	return work.New(pool), pool
}

func mustCreate(t *testing.T, svc *work.Service, in work.NewItem) db.WorkItem {
	t.Helper()
	res, err := svc.Upsert(context.Background(), in, user)
	if err != nil {
		t.Fatalf("create %+v: %v", in, err)
	}
	return res.Item
}

// forceState puts an item in a state without going through the state machine,
// so a test can start from any state.
func forceState(t *testing.T, pool *pgxpool.Pool, id, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET state = $2 WHERE id = $1`, id, state); err != nil {
		t.Fatal(err)
	}
}

func TestTransitionTable(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	allowed := map[string][]string{
		work.Design:     {work.Ready, work.Blocked, work.WontFix},
		work.Ready:      {work.InProgress, work.Design, work.Blocked, work.WontFix},
		work.InProgress: {work.InReview, work.Ready, work.Blocked, work.Failed, work.Completed},
		work.InReview:   {work.InProgress, work.Completed, work.Blocked, work.Failed, work.WontFix},
		work.Blocked:    {work.Design, work.Ready, work.WontFix, work.Completed},
		work.Failed:     {work.Ready, work.WontFix, work.Completed},
		work.Completed:  {work.Design, work.Ready},
		work.WontFix:    {work.Design, work.Ready},
	}
	for _, from := range work.States() {
		for _, to := range work.States() {
			want := slices.Contains(allowed[from], to)
			t.Run(from+"->"+to, func(t *testing.T) {
				item := mustCreate(t, svc, work.NewItem{Title: "t"})
				// A merged output, so the completion rule never stands in the way of the table.
				if _, err := svc.RecordOutput(ctx, item.ID, work.OutputInput{Repo: "r", Kind: work.KindCommit, SHA: from + to}, user); err != nil {
					t.Fatal(err)
				}
				forceState(t, pool, item.ID, from)
				got, err := svc.Transition(ctx, item.ID, to, "because", user)
				if !want {
					var te *work.TransitionError
					if !errors.Is(err, work.ErrIllegalTransition) || !errors.As(err, &te) {
						t.Fatalf("err = %v, want ErrIllegalTransition", err)
					}
					if te.From != from || !slices.Equal(te.Allowed, work.AllowedTargets(from)) {
						t.Errorf("error names %q and %v, want %q and %v", te.From, te.Allowed, from, work.AllowedTargets(from))
					}
					return
				}
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if got.State != to || got.StateReason != "because" {
					t.Errorf("item state %q reason %q, want %q and \"because\"", got.State, got.StateReason, to)
				}
				events, err := svc.Events(ctx, item.ID)
				if err != nil {
					t.Fatal(err)
				}
				last := events[len(events)-1]
				if last.Type != work.EventTransition || *last.FromState != from || *last.ToState != to || last.ActorKind != work.ActorUser {
					t.Errorf("last event %+v, want a transition from %s to %s by a user", last, from, to)
				}
			})
		}
	}
	if got := work.AllowedTargets("nonsense"); got != nil {
		t.Errorf("AllowedTargets(nonsense) = %v, want nil", got)
	}
}

func TestUpsertIsIdempotentAndNeverRegresses(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	in := work.NewItem{Source: "github", ExternalID: "o/r#1", Title: "first", State: work.Design}

	first, err := svc.Upsert(ctx, in, user)
	if err != nil || !first.Created {
		t.Fatalf("first upsert: %+v, %v; want created", first, err)
	}
	in.Title = "second"
	in.State = work.Ready
	again, err := svc.Upsert(ctx, in, user)
	if err != nil || again.Created || again.Item.ID != first.Item.ID {
		t.Fatalf("second upsert: %+v, %v; want the same item, not created", again, err)
	}
	if again.Item.Title != "second" || again.Item.State != work.Design || !again.Changed {
		t.Errorf("second upsert: title %q state %q changed %v; want the new title, state left at design", again.Item.Title, again.Item.State, again.Changed)
	}

	if _, err := svc.Transition(ctx, first.Item.ID, work.Ready, "", user); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Transition(ctx, first.Item.ID, work.InProgress, "", worker); err != nil {
		t.Fatal(err)
	}
	in.Title = "third"
	frozen, err := svc.Upsert(ctx, in, user)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Changed || frozen.Reason == "" || frozen.Item.Title != "second" || frozen.Item.State != work.InProgress {
		t.Errorf("upsert of claimed work: %+v; want unchanged with a reason", frozen)
	}

	items, _, err := svc.List(ctx, work.Filter{Source: "github"})
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %d items, %v; want exactly one", len(items), err)
	}
}

func TestUpsertConcurrentlyCreatesOne(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	var wg sync.WaitGroup
	created := make(chan bool, 10)
	for range 10 {
		wg.Go(func() {
			res, err := svc.Upsert(ctx, work.NewItem{Source: "jira", ExternalID: "X-1", Title: "t"}, user)
			if err != nil {
				t.Error(err)
				return
			}
			created <- res.Created
		})
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d upserts reported created, want 1", n)
	}
}

func TestValidation(t *testing.T) {
	svc, _ := newService(t)
	tests := []struct {
		name string
		in   work.NewItem
	}{
		{name: "no title", in: work.NewItem{}},
		{name: "source without external id", in: work.NewItem{Title: "t", Source: "github"}},
		{name: "external id without source", in: work.NewItem{Title: "t", ExternalID: "x"}},
		{name: "initial state in_progress", in: work.NewItem{Title: "t", State: work.InProgress}},
		{name: "empty repo", in: work.NewItem{Title: "t", Repos: []string{""}}},
		{name: "title too long", in: work.NewItem{Title: string(make([]byte, work.MaxTitleBytes+1))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Upsert(context.Background(), tt.in, user); !errors.Is(err, work.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestConcurrentTransitionsAreConsistent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t"})

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := svc.Transition(ctx, item.ID, work.InProgress, "", worker)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var ok, illegal int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, work.ErrIllegalTransition):
			illegal++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || illegal != 1 {
		t.Fatalf("%d succeeded and %d were illegal, want 1 and 1", ok, illegal)
	}
	events, err := svc.Events(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != work.EventCreated || events[1].Type != work.EventTransition {
		t.Fatalf("events = %+v, want created then one transition", events)
	}
}

func TestHistoryReadsBackInOrder(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t", State: work.Design})
	path := []string{work.Ready, work.Blocked, work.Ready, work.InProgress, work.Failed, work.Ready}
	for _, to := range path {
		if _, err := svc.Transition(ctx, item.ID, to, "", user); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	events, err := svc.Events(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(path)+1 {
		t.Fatalf("%d events, want %d", len(events), len(path)+1)
	}
	from := work.Design
	for i, to := range path {
		e := events[i+1]
		if *e.FromState != from || *e.ToState != to {
			t.Errorf("event %d: %s->%s, want %s->%s", i+1, *e.FromState, *e.ToState, from, to)
		}
		if e.ID <= events[i].ID || e.At.Before(events[i].At) {
			t.Errorf("event %d is out of order", i+1)
		}
		from = to
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t"})
	if _, err := pool.Exec(ctx, `UPDATE work_events SET type = 'x' WHERE work_item_id = $1`, item.ID); err == nil {
		t.Error("updated a work event")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM work_events WHERE work_item_id = $1`, item.ID); err == nil {
		t.Error("deleted a work event")
	}
}

func TestStateIsConstrainedInTheDatabase(t *testing.T) {
	svc, pool := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t"})
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET state = 'claimed' WHERE id = $1`, item.ID); err == nil {
		t.Fatal("the database accepted an unknown state")
	}
}

func TestTwoPullRequestsInTwoRepos(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a", "o/b"}})
	if _, err := svc.Transition(ctx, item.ID, work.InProgress, "", worker); err != nil {
		t.Fatal(err)
	}
	prA := work.OutputInput{Repo: "o/a", Kind: work.KindPullRequest, URL: "https://x/o/a/pull/1", Branch: "b", Status: work.OutputOpen}
	prB := work.OutputInput{Repo: "o/b", Kind: work.KindPullRequest, URL: "https://x/o/b/pull/7", Branch: "b", Status: work.OutputOpen}
	for _, pr := range []work.OutputInput{prA, prB} {
		if _, err := svc.RecordOutput(ctx, item.ID, pr, worker); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		got, err := svc.Get(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	if got := state(); got != work.InProgress {
		t.Fatalf("while the worker is still producing: %s, want in_progress", got)
	}
	if _, err := svc.FinishOutputs(ctx, item.ID, worker); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != work.InReview {
		t.Fatalf("after the worker finished with two open pull requests: %s, want in_review", got)
	}
	prA.Status = work.OutputMerged
	if _, err := svc.RecordOutput(ctx, item.ID, prA, user); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != work.InReview {
		t.Fatalf("after the first merge: %s, want in_review", got)
	}
	if _, err := svc.Transition(ctx, item.ID, work.Completed, "", user); !errors.Is(err, work.ErrOutputsPending) {
		t.Fatalf("completing with an open output: %v, want ErrOutputsPending", err)
	}
	prB.Status = work.OutputMerged
	if _, err := svc.RecordOutput(ctx, item.ID, prB, user); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != work.Completed {
		t.Fatalf("after the second merge: %s, want completed", got)
	}
	outputs, err := svc.Outputs(ctx, item.ID)
	if err != nil || len(outputs) != 2 {
		t.Fatalf("outputs: %d, %v; want 2", len(outputs), err)
	}
}

func TestCompletionRules(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	bare := mustCreate(t, svc, work.NewItem{Title: "no outputs"})
	if _, err := svc.Transition(ctx, bare.ID, work.InProgress, "", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Transition(ctx, bare.ID, work.Completed, "", user); !errors.Is(err, work.ErrOutputsPending) {
		t.Errorf("completing with no outputs: %v, want ErrOutputsPending", err)
	}
	if _, err := svc.FinishOutputs(ctx, bare.ID, worker); !errors.Is(err, work.ErrNoOutputs) {
		t.Errorf("finishing with no outputs: %v, want ErrNoOutputs", err)
	}

	commit := mustCreate(t, svc, work.NewItem{Title: "direct commit"})
	if _, err := svc.Transition(ctx, commit.ID, work.InProgress, "", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordOutput(ctx, commit.ID, work.OutputInput{Repo: "o/a", Kind: work.KindCommit, SHA: "deadbeef"}, worker); err != nil {
		t.Fatal(err)
	}
	got, err := svc.FinishOutputs(ctx, commit.ID, worker)
	if err != nil || got.State != work.Completed {
		t.Fatalf("finishing with one merged commit: %s, %v; want completed", got.State, err)
	}

	// A closed pull request does not hold the item back once another output has landed.
	closed := mustCreate(t, svc, work.NewItem{Title: "superseded"})
	if _, err := svc.Transition(ctx, closed.ID, work.InProgress, "", worker); err != nil {
		t.Fatal(err)
	}
	for _, o := range []work.OutputInput{
		{Repo: "o/a", Kind: work.KindPullRequest, URL: "u1", Status: work.OutputClosed},
		{Repo: "o/a", Kind: work.KindPullRequest, URL: "u2", Status: work.OutputMerged},
	} {
		if _, err := svc.RecordOutput(ctx, closed.ID, o, worker); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := svc.FinishOutputs(ctx, closed.ID, worker); err != nil || got.State != work.Completed {
		t.Fatalf("closed plus merged: %s, %v; want completed", got.State, err)
	}

	// Another item cannot take over an output.
	if _, err := svc.RecordOutput(ctx, bare.ID, work.OutputInput{Repo: "o/a", Kind: work.KindPullRequest, URL: "u1", Status: work.OutputOpen}, worker); !errors.Is(err, work.ErrOutputConflict) {
		t.Errorf("recording another item's output: %v, want ErrOutputConflict", err)
	}
	if _, err := svc.RecordOutput(ctx, bare.ID, work.OutputInput{Repo: "o/a", Kind: "patch"}, worker); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("unknown output kind: %v, want ErrInvalid", err)
	}
}

func TestUnfinishedDependenciesBlockStart(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	dep := mustCreate(t, svc, work.NewItem{Title: "dependency"})
	item := mustCreate(t, svc, work.NewItem{Title: "dependent"})

	if err := svc.AddDependency(ctx, item.ID, item.ID, user); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("self dependency: %v, want ErrInvalid", err)
	}
	if err := svc.AddDependency(ctx, item.ID, "00000000-0000-0000-0000-000000000000", user); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("unknown dependency: %v, want ErrNotFound", err)
	}
	if err := svc.AddDependency(ctx, item.ID, dep.ID, user); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDependency(ctx, item.ID, dep.ID, user); err != nil {
		t.Fatalf("adding the same dependency twice: %v", err)
	}
	if _, err := svc.Transition(ctx, item.ID, work.InProgress, "", worker); !errors.Is(err, work.ErrUnfinishedDependencies) {
		t.Fatalf("starting with an unfinished dependency: %v, want ErrUnfinishedDependencies", err)
	}
	if got, _ := svc.Get(ctx, item.ID); got.State != work.Ready {
		t.Errorf("dependent is %s, want it to stay ready", got.State)
	}
	if _, err := svc.Transition(ctx, dep.ID, work.WontFix, "", user); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Transition(ctx, item.ID, work.InProgress, "", worker); err != nil {
		t.Fatalf("starting once the dependency is finished: %v", err)
	}
	deps, err := svc.Dependencies(ctx, item.ID)
	if err != nil || len(deps) != 1 || deps[0] != dep.ID {
		t.Fatalf("dependencies = %v, %v", deps, err)
	}
	if err := svc.RemoveDependency(ctx, item.ID, dep.ID, user); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDependency(ctx, item.ID, dep.ID, user); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("removing a missing dependency: %v, want ErrNotFound", err)
	}
}

func TestGetAndPatch(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if _, err := svc.Get(ctx, id); !errors.Is(err, work.ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
	item := mustCreate(t, svc, work.NewItem{Title: "t", Body: "b"})
	title, priority := "new", int32(5)
	got, err := svc.Patch(ctx, item.ID, work.Patch{Title: &title, Priority: &priority}, user)
	if err != nil || got.Title != "new" || got.Body != "b" || got.Priority != 5 {
		t.Fatalf("patch: %+v, %v", got, err)
	}
	empty := ""
	if _, err := svc.Patch(ctx, item.ID, work.Patch{Title: &empty}, user); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("empty title: %v, want ErrInvalid", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	for i := range 5 {
		mustCreate(t, svc, work.NewItem{Title: "t", Priority: int32(i % 2), Repos: []string{"o/a"}})
	}
	mustCreate(t, svc, work.NewItem{Title: "other", State: work.Design, Repos: []string{"o/b"}})

	var seen []db.WorkItem
	cursor := ""
	for {
		page, next, err := svc.List(ctx, work.Filter{State: work.Ready, Repo: "o/a", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d items, want 5", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].Priority > seen[i-1].Priority {
			t.Errorf("item %d has higher priority than the one before it", i)
		}
	}
	if _, _, err := svc.List(ctx, work.Filter{Cursor: "garbage"}); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("bad cursor: %v, want ErrInvalid", err)
	}
	if _, _, err := svc.List(ctx, work.Filter{State: "nope"}); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("bad state filter: %v, want ErrInvalid", err)
	}
}

func TestIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	in := work.NewItem{Title: "cron job", IdempotencyKey: "run-42"}
	first, err := svc.Upsert(ctx, in, user)
	if err != nil || !first.Created {
		t.Fatalf("first: %+v, %v", first, err)
	}
	again, err := svc.Upsert(ctx, in, user)
	if err != nil || again.Created || again.Item.ID != first.Item.ID {
		t.Fatalf("repeat: %+v, %v; want the first item", again, err)
	}
	other, err := svc.Upsert(ctx, work.NewItem{Title: "cron job"}, user)
	if err != nil || !other.Created || other.Item.ID == first.Item.ID {
		t.Fatalf("no key: %+v, %v; want a new item", other, err)
	}
}

func TestUpsertWithBlockedBy(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	byID := mustCreate(t, svc, work.NewItem{Title: "a"})
	bySource := mustCreate(t, svc, work.NewItem{Title: "b", Source: "github", ExternalID: "o/r#9"})
	res, err := svc.Upsert(ctx, work.NewItem{Title: "c", BlockedBy: []string{byID.ID, "github:o/r#9"}}, user)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := svc.Dependencies(ctx, res.Item.ID)
	if err != nil || len(deps) != 2 || !slices.Contains(deps, byID.ID) || !slices.Contains(deps, bySource.ID) {
		t.Fatalf("dependencies = %v, %v", deps, err)
	}
	if _, err := svc.Upsert(ctx, work.NewItem{Title: "d", BlockedBy: []string{"github:o/r#404"}}, user); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("unknown blocked_by: %v, want ErrNotFound", err)
	}
	items, _, _ := svc.List(ctx, work.Filter{})
	if len(items) != 3 {
		t.Fatalf("%d items; the failed upsert must create nothing", len(items))
	}
}
