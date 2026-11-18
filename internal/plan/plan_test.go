package plan_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/plan"
)

// updateGoldenEnv rewrites the golden files instead of comparing against them
// (make generate/golden).
const updateGoldenEnv = "FACTORY_UPDATE_GOLDEN"

const itemID = "b2e9c0de-0000-4000-8000-000000000001"

func TestGolden(t *testing.T) {
	ordered := plan.DefaultRepoConfig()
	ordered.After = []string{"o/lib"}
	ordered.RequireApproval = true
	cases := []struct {
		name    string
		repos   []string
		configs map[string]plan.RepoConfig
	}{
		{"one-repo", []string{"o/quark"}, nil},
		{"two-independent-repos", []string{"o/quark", "o/web"}, nil},
		{"two-ordered-repos", []string{"o/lib", "o/app"}, map[string]plan.RepoConfig{"o/app": ordered}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := plan.Plan(plan.Item{ID: itemID, Title: "Add null check to the importer", Repos: c.repos}, c.configs, plan.DefaultTemplate())
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(g, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", c.name+".golden.json")
			if os.Getenv(updateGoldenEnv) != "" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `make generate/golden` to write it)", err)
			}
			if string(got) != string(want) {
				t.Fatalf("graph differs from %s (run `make generate/golden` if the change is intended):\n%s", path, got)
			}
			again, _ := plan.Plan(plan.Item{ID: itemID, Title: "Add null check to the importer", Repos: c.repos}, c.configs, plan.DefaultTemplate())
			if again.InputsHash != g.InputsHash {
				t.Fatal("same inputs gave a different inputs_hash")
			}
		})
	}
}

func TestInputsHashChangesWithInputs(t *testing.T) {
	base, _ := plan.Plan(plan.Item{ID: itemID, Repos: []string{"o/a"}}, nil, plan.DefaultTemplate())
	cfg := plan.DefaultRepoConfig()
	cfg.ReviewRounds = 1
	other, _ := plan.Plan(plan.Item{ID: itemID, Repos: []string{"o/a"}}, map[string]plan.RepoConfig{"o/a": cfg}, plan.DefaultTemplate())
	if base.InputsHash == other.InputsHash || base.InputsHash == "" {
		t.Fatalf("hashes %q and %q; want different, non-empty", base.InputsHash, other.InputsHash)
	}
}

// TestProperties plans many combinations of repositories and settings and
// checks every graph: no cycle, no duplicate key, no edge to a missing step,
// conditions only on ancestors.
func TestProperties(t *testing.T) {
	repos := []string{"o/a", "o/b", "o/c", "o/d"}
	n := 0
	for mask := 1; mask < 1<<len(repos); mask++ {
		var picked []string
		for i, r := range repos {
			if mask&(1<<i) != 0 {
				picked = append(picked, r)
			}
		}
		for rounds := range 4 {
			for _, approval := range []bool{false, true} {
				for _, merge := range []bool{false, true} {
					configs := map[string]plan.RepoConfig{}
					for i, r := range picked {
						cfg := plan.RepoConfig{ReviewRounds: rounds, RequireApproval: approval, Merge: merge}
						if i > 0 && (mask+rounds)%2 == 0 {
							cfg.After = picked[:i] // every earlier repo: ordered, never cyclic
						}
						configs[r] = cfg
					}
					g, err := plan.Plan(plan.Item{ID: itemID, Repos: picked}, configs, plan.DefaultTemplate())
					if err != nil {
						t.Fatalf("%v rounds=%d: %v", picked, rounds, err)
					}
					checkGraph(t, g)
					n++
				}
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d graphs checked", n)
	}
}

// checkGraph is the property, checked independently of Validate.
func checkGraph(t *testing.T, g plan.Graph) {
	t.Helper()
	byKey := map[string]plan.Step{}
	for _, s := range g.Steps {
		if _, dup := byKey[s.Key]; dup || s.Key == "" {
			t.Fatalf("duplicate or empty key %q", s.Key)
		}
		byKey[s.Key] = s
	}
	for _, s := range g.Steps {
		for _, d := range s.DependsOn {
			if _, ok := byKey[d]; !ok {
				t.Fatalf("%s depends on missing %s", s.Key, d)
			}
		}
	}
	// Every step must be reachable in a topological order.
	done := map[string]bool{}
	for len(done) < len(g.Steps) {
		progress := false
		for _, s := range g.Steps {
			if done[s.Key] {
				continue
			}
			ok := true
			for _, d := range s.DependsOn {
				ok = ok && done[d]
			}
			if ok {
				done[s.Key], progress = true, true
			}
		}
		if !progress {
			t.Fatal("cycle in graph")
		}
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsBadGraphs(t *testing.T) {
	step := func(key string, deps ...string) plan.Step {
		return plan.Step{Key: key, Kind: plan.Gate, DependsOn: deps}
	}
	cases := map[string][]plan.Step{
		"duplicate key": {step("a"), step("a")},
		"missing dep":   {step("a", "b")},
		"self edge":     {step("a", "a")},
		"cycle":         {step("a", "c"), step("b", "a"), step("c", "b")},
		"unknown kind":  {{Key: "a", Kind: "dance"}},
		"condition not on an ancestor": {
			step("a"), {Key: "b", Kind: plan.Gate, Condition: "c == pass"}, step("c"),
		},
	}
	for name, steps := range cases {
		if err := (plan.Graph{Steps: steps}).Validate(); !errors.Is(err, plan.ErrInvalidGraph) {
			t.Errorf("%s: %v, want ErrInvalidGraph", name, err)
		}
	}
}

func TestPlanErrors(t *testing.T) {
	after := plan.DefaultRepoConfig()
	after.After = []string{"o/missing"}
	cyclicA, cyclicB := plan.DefaultRepoConfig(), plan.DefaultRepoConfig()
	cyclicA.After, cyclicB.After = []string{"o/b"}, []string{"o/a"}
	cases := map[string]struct {
		item    plan.Item
		configs map[string]plan.RepoConfig
		tmpl    plan.Template
	}{
		"no repos":        {plan.Item{ID: itemID}, nil, plan.DefaultTemplate()},
		"after unknown":   {plan.Item{ID: itemID, Repos: []string{"o/a"}}, map[string]plan.RepoConfig{"o/a": after}, plan.DefaultTemplate()},
		"ordering cycle":  {plan.Item{ID: itemID, Repos: []string{"o/a", "o/b"}}, map[string]plan.RepoConfig{"o/a": cyclicA, "o/b": cyclicB}, plan.DefaultTemplate()},
		"fix in chain":    {plan.Item{ID: itemID, Repos: []string{"o/a"}}, nil, plan.Template{Name: "x", Chain: []string{plan.Fix}}},
		"negative rounds": {plan.Item{ID: itemID, Repos: []string{"o/a"}}, map[string]plan.RepoConfig{"o/a": {ReviewRounds: -1}}, plan.DefaultTemplate()},
	}
	for name, c := range cases {
		if _, err := plan.Plan(c.item, c.configs, c.tmpl); !errors.Is(err, plan.ErrInvalidGraph) {
			t.Errorf("%s: %v, want ErrInvalidGraph", name, err)
		}
	}
}

func keysOf(g plan.Graph) []string {
	keys := make([]string, len(g.Steps))
	for i, s := range g.Steps {
		keys[i] = s.Key
	}
	return keys
}

func TestReviewRoundsAreUnrolled(t *testing.T) {
	g, err := plan.Plan(plan.Item{ID: itemID, Repos: []string{"o/a"}}, nil, plan.DefaultTemplate())
	if err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(keysOf(g), " ")
	for _, want := range []string{"o/a/review-1 o/a/fix-1 o/a/review-2 o/a/fix-2 o/a/review-3 ", "o/a/await_checks-1 o/a/fix-checks-1 o/a/await_checks-2 o/a/merge"} {
		if !strings.Contains(keys, want) {
			t.Errorf("keys %q lack %q", keys, want)
		}
	}
	none := plan.DefaultRepoConfig()
	none.ReviewRounds, none.Merge = 0, false
	g, _ = plan.Plan(plan.Item{ID: itemID, Repos: []string{"o/a"}}, map[string]plan.RepoConfig{"o/a": none}, plan.DefaultTemplate())
	for _, k := range keysOf(g) {
		if strings.Contains(k, "review") || strings.HasSuffix(k, "merge") {
			t.Errorf("review and merge are off, but the graph has %s", k)
		}
	}
}

// status builds the scheduler's view: every step pending except those named.
func status(g plan.Graph, set map[string]plan.Status) map[string]plan.Status {
	out := map[string]plan.Status{}
	for _, s := range g.Steps {
		out[s.Key] = plan.Status{State: plan.Pending}
	}
	for k, v := range set {
		out[k] = v
	}
	return out
}

func TestNext(t *testing.T) {
	cfg := plan.DefaultRepoConfig()
	cfg.ReviewRounds = 2
	g, err := plan.Plan(plan.Item{ID: itemID, Repos: []string{"o/a", "o/b"}}, map[string]plan.RepoConfig{"o/a": cfg, "o/b": cfg}, plan.DefaultTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Next(g, status(g, nil)); fmt.Sprint(got) != "map[o/a/workspace:ready o/b/workspace:ready]" {
		t.Fatalf("roots: %v", got)
	}
	done := plan.Status{State: plan.Succeeded}
	upToReview := map[string]plan.Status{}
	for _, k := range []string{"workspace", "implement", "gate", "secret_scan", "open_pr"} {
		upToReview["o/a/"+k] = done
	}

	passed := with(upToReview, "o/a/review-1", plan.Status{State: plan.Succeeded, Verdict: "pass"})
	if got := plan.Next(g, status(g, passed)); fmt.Sprint(got) != "map[o/a/await_checks-1:ready o/a/fix-1:skipped o/a/review-2:skipped o/b/workspace:ready]" {
		t.Errorf("review passed: %v", got)
	}
	changes := with(upToReview, "o/a/review-1", plan.Status{State: plan.Succeeded, Verdict: "changes"})
	if got := plan.Next(g, status(g, changes)); fmt.Sprint(got) != "map[o/a/fix-1:ready o/b/workspace:ready]" {
		t.Errorf("changes requested: %v", got)
	}
	failed := with(upToReview, "o/a/review-1", plan.Status{State: plan.Failed})
	if got := plan.Next(g, status(g, failed)); fmt.Sprint(got) != "map[o/b/workspace:ready]" {
		t.Errorf("after a failure nothing downstream moves: %v", got)
	}
}

func with(base map[string]plan.Status, k string, v plan.Status) map[string]plan.Status {
	out := map[string]plan.Status{k: v}
	for bk, bv := range base {
		out[bk] = bv
	}
	return out
}

func TestRoundsExhausted(t *testing.T) {
	cases := []struct {
		params, result string
		want           bool
	}{
		{`{"last":true}`, `{"verdict":"changes"}`, true},
		{`{"last":true}`, `{"verdict":"fail"}`, true},
		{`{"last":true}`, `{"verdict":"pass"}`, false},
		{`{"last":false}`, `{"verdict":"changes"}`, false},
		{`{}`, `{"verdict":"changes"}`, false},
		{`{"last":true}`, `null`, false},
		{`{"last":true}`, ``, false},
	}
	for _, c := range cases {
		if got := plan.RoundsExhausted(json.RawMessage(c.params), json.RawMessage(c.result)); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.params, c.result, got, c.want)
		}
	}
}

func TestKinds(t *testing.T) {
	for _, k := range plan.Kinds() {
		wait := slices.Contains(plan.WaitKinds(), k)
		if wait != plan.IsWait(k) {
			t.Errorf("%s: IsWait %v", k, plan.IsWait(k))
		}
	}
	if !plan.IsWait(plan.AwaitChecks) || !plan.IsWait(plan.AwaitApproval) || plan.IsWait(plan.Merge) {
		t.Error("only await_checks and await_approval are wait steps")
	}
}
