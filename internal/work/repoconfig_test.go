package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/repoconfig"
	"github.com/autobutler-org/factory/internal/work"
)

const quarkImage = "ghcr.io/autobutler-org/quark-worker:1.4.0"

// hosts serves one codehost.Fake per repository; a repository without one
// cannot be reached.
func hosts(fakes map[string]*codehost.Fake) work.Configs {
	return work.Configs{
		Host: func(_ context.Context, repo string) (codehost.Host, error) {
			f, ok := fakes[repo]
			if !ok {
				return nil, fmt.Errorf("mint a token for %s: connection refused", repo)
			}
			return f, nil
		},
		AllowedImages: []string{"ghcr.io/autobutler-org/"},
	}
}

func withConfig(content string) *codehost.Fake {
	return &codehost.Fake{Files: map[string]string{repoconfig.Path: content}}
}

func lastEvent(t *testing.T, svc *work.Service, id, typ string) map[string]any {
	t.Helper()
	events, err := svc.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			var payload map[string]any
			if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			return payload
		}
	}
	t.Fatalf("no %s event", typ)
	return nil
}

func TestStartReadyPlansWithTheRepositoryConfig(t *testing.T) {
	ctx := context.Background()
	base, _ := newService(t)
	svc := base.WithConfigs(hosts(map[string]*codehost.Fake{
		"o/quark": withConfig("gate = \"make check\"\nreview_rounds = 1\nimage = \"" + quarkImage + "\"\n"),
		"o/plain": withConfig("gate = \"make check\"\n"),
	}))
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/quark", "o/plain"}})
	if n, err := svc.StartReady(ctx, launched, 1); err != nil || n != 1 {
		t.Fatalf("StartReady = %d, %v", n, err)
	}
	got := states(t, svc, item.ID)
	if _, ok := got["o/quark/review-1"]; !ok || got["o/quark/review-2"] != "" || got["o/plain/review-3"] == "" {
		t.Errorf("graph %v: want one review round for o/quark and the default three for o/plain", got)
	}
	g, err := svc.Graph(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range g.Steps {
		var p struct {
			Image string `json:"image"`
		}
		_ = json.Unmarshal(s.Params, &p)
		if want := map[string]string{"o/quark/workspace": quarkImage}[s.Key]; p.Image != want {
			t.Errorf("%s image = %q, want %q", s.Key, p.Image, want)
		}
	}
	quark := plan.DefaultRepoConfig()
	quark.ReviewRounds, quark.Image = 1, quarkImage
	want, _ := plan.Plan(plan.Item{ID: item.ID, Title: item.Title, Repos: item.Repos},
		map[string]plan.RepoConfig{"o/quark": quark, "o/plain": plan.DefaultRepoConfig()}, plan.DefaultTemplate())
	if g.Graph.InputsHash != want.InputsHash {
		t.Error("inputs_hash is not the hash of the repositories' configs")
	}
	reads, _ := json.Marshal(lastEvent(t, svc, item.ID, work.EventPlanned)["configs"])
	for _, s := range []string{`"repo":"o/quark"`, `"commit":"` + codehost.FakeCommit + `"`, `"image":"` + quarkImage + `"`, `"branch":"main"`, `"repo":"o/plain"`} {
		if !strings.Contains(string(reads), s) {
			t.Errorf("planned event configs %s lack %s", reads, s)
		}
	}
}

func TestStartReadyBlocksOnAnUnusableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		host *codehost.Fake
		want string
	}{
		{"missing", &codehost.Fake{}, ".factory/config.toml not found on main"},
		{"invalid", withConfig("gate = \"x\"\nreview_round = 1\n"), "unknown key(s) review_round"},
		{"disallowed image", withConfig("gate = \"x\"\nimage = \"ghcr.io/autobutler-org.evil.example/x:1\"\n"), "image is not allowed"},
		{"malformed image", withConfig("gate = \"x\"\nimage = \"quark\"\n"), "malformed image reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := newService(t)
			svc := base.WithConfigs(hosts(map[string]*codehost.Fake{"o/a": tc.host}))
			item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
			if n, err := svc.StartReady(context.Background(), launched, 5); err != nil || n != 1 {
				t.Fatalf("StartReady = %d, %v; want the item taken (to blocked)", n, err)
			}
			got, err := svc.Get(context.Background(), item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != work.Blocked || !strings.Contains(got.StateReason, tc.want) || !strings.Contains(got.StateReason, "o/a") {
				t.Errorf("item %s (%q); want blocked saying %q about o/a", got.State, got.StateReason, tc.want)
			}
			if len(states(t, svc, item.ID)) != 0 {
				t.Error("an item with an unusable config was planned")
			}
		})
	}
}

func TestStartReadyLeavesTheItemReadyWhenTheCodeHostIsDown(t *testing.T) {
	ctx := context.Background()
	base, pool := newService(t)
	down := &codehost.Fake{ReadErr: errors.New("502 Bad Gateway")}
	svc := base.WithConfigs(hosts(map[string]*codehost.Fake{"o/down": down, "o/up": withConfig("gate = \"x\"\n")}))
	high := mustCreate(t, svc, work.NewItem{Title: "high", Repos: []string{"o/down"}, Priority: 5})
	unreachable := mustCreate(t, svc, work.NewItem{Title: "no host", Repos: []string{"o/none"}, Priority: 4})
	low := mustCreate(t, svc, work.NewItem{Title: "low", Repos: []string{"o/up"}})
	n, err := svc.StartReady(ctx, launched, 5)
	if err != nil || n != 1 {
		t.Fatalf("StartReady = %d, %v; want only the reachable item planned", n, err)
	}
	if itemState(t, svc, high.ID) != work.Ready || itemState(t, svc, unreachable.ID) != work.Ready || itemState(t, svc, low.ID) != work.InProgress {
		t.Error("an outage must leave its item ready and not hold up the others")
	}
	if reason, _ := lastEvent(t, svc, high.ID, work.EventPlanDeferred)["reason"].(string); !strings.Contains(reason, "502") {
		t.Errorf("plan_deferred reason = %q, want the outage", reason)
	}

	// The item is put off for the retry delay; end it now.
	if _, err := pool.Exec(ctx, `UPDATE work_items SET available_at = now() WHERE id = $1`, high.ID); err != nil {
		t.Fatal(err)
	}
	down.ReadErr = nil
	down.Files = map[string]string{repoconfig.Path: "gate = \"x\"\n"}
	if _, err := base.WithConfigs(hosts(map[string]*codehost.Fake{"o/down": down})).StartReady(ctx, launched, 5); err != nil {
		t.Fatal(err)
	}
	if itemState(t, svc, high.ID) != work.InProgress {
		t.Error("the item was not planned once the code host was back")
	}
}

func TestClaimStepPlansWithTheRepositoryConfig(t *testing.T) {
	ctx := context.Background()
	base, _ := newService(t)
	svc := base.WithConfigs(hosts(map[string]*codehost.Fake{"o/a": withConfig("gate = \"x\"\nreview_rounds = 0\n")}))
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	w, err := svc.RegisterWorker(ctx, work.WorkerInput{Name: "w", Kind: work.WorkerCluster})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.ClaimStep(ctx, w.ID, ""); err != nil || !ok {
		t.Fatalf("ClaimStep = %v, %v", ok, err)
	}
	for k := range states(t, svc, item.ID) {
		if strings.Contains(k, "review") {
			t.Errorf("review_rounds = 0, but the graph has %s", k)
		}
	}
}

func TestReplanReadsTheConfigAgain(t *testing.T) {
	ctx := context.Background()
	base, _ := newService(t)
	host := withConfig("gate = \"x\"\n")
	svc := base.WithConfigs(hosts(map[string]*codehost.Fake{"o/a": host}))
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	if _, err := svc.StartReady(ctx, launched, 1); err != nil {
		t.Fatal(err)
	}
	host.Files[repoconfig.Path] = "gate = \"x\"\nreview_rounds = 1\n"
	view, err := svc.Replan(ctx, item.ID, user)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(view.Steps); n != len(mustPlanKeys(t, item, 1)) {
		t.Errorf("replanned graph has %d steps, want the one-round plan", n)
	}
	host.Files[repoconfig.Path] = "nope"
	if _, err := svc.Replan(ctx, item.ID, user); !errors.Is(err, work.ErrInvalid) || !strings.Contains(err.Error(), "o/a") {
		t.Errorf("Replan with a broken config = %v, want ErrInvalid naming o/a", err)
	}
	host.ReadErr = errors.New("timeout")
	if _, err := svc.Replan(ctx, item.ID, user); !errors.Is(err, work.ErrConfigUnavailable) {
		t.Errorf("Replan during an outage = %v, want ErrConfigUnavailable", err)
	}
}

func mustPlanKeys(t *testing.T, item db.WorkItem, rounds int) []plan.Step {
	t.Helper()
	cfg := plan.DefaultRepoConfig()
	cfg.ReviewRounds = rounds
	g, err := plan.Plan(plan.Item{ID: item.ID, Title: item.Title, Repos: item.Repos}, map[string]plan.RepoConfig{item.Repos[0]: cfg}, plan.DefaultTemplate())
	if err != nil {
		t.Fatal(err)
	}
	return g.Steps
}

func TestSandboxImage(t *testing.T) {
	ctx := context.Background()
	base, _ := newService(t)
	svc := base.WithConfigs(hosts(map[string]*codehost.Fake{
		"o/quark": withConfig("gate = \"x\"\nimage = \"" + quarkImage + "\"\n"),
		"o/plain": withConfig("gate = \"x\"\n"),
	}))
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/quark", "o/plain"}})
	if _, err := svc.StartReady(ctx, launched, 1); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{item.ID + "/o/quark": quarkImage, item.ID + "/o/plain": "", "nope/o/x": ""} {
		if got, err := svc.SandboxImage(ctx, key); err != nil || got != want {
			t.Errorf("SandboxImage(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
}
