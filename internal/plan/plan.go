// Package plan turns a work item into a deterministic graph of steps and
// decides, from the steps' states, which ones may run next.
//
// Plan is a pure function: no clock, randomness, network or model call. The
// same item, repository configs and template give the same graph, byte for
// byte, and Graph.InputsHash records what it was built from. Next is the
// scheduler's rule, equally pure. The package has no SQL and does not run
// steps: internal/work stores graphs and leases steps to workers, and the
// worker (#11) implements each kind. See docs/architecture/graph.md.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Step kinds.
const (
	Workspace     = "workspace"
	Implement     = "implement"
	Gate          = "gate"
	SecretScan    = "secret_scan"
	OpenPR        = "open_pr"
	Review        = "review"
	Fix           = "fix"
	AwaitChecks   = "await_checks"
	AwaitApproval = "await_approval"
	Merge         = "merge"
)

// Step states.
const (
	Pending   = "pending"
	Ready     = "ready"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Skipped   = "skipped"
	Canceled  = "canceled"
)

// Verdicts a review or await_checks step reports in its result. Pass moves
// on; any other verdict triggers the next fix round, or fails the step on the
// last round.
const (
	VerdictPass    = "pass"
	VerdictChanges = "changes"
	VerdictFail    = "fail"
)

// checkRounds is how many times the chain waits for checks: one fix round
// for failing checks, as schmeide had.
const checkRounds = 2

// ErrInvalidGraph is returned for inputs that cannot be planned and for a
// graph that breaks the rules: duplicate keys, missing dependencies, cycles,
// unknown kinds, or conditions on steps that are not ancestors.
var ErrInvalidGraph = errors.New("invalid graph")

// Kinds lists every step kind.
func Kinds() []string {
	return []string{Workspace, Implement, Gate, SecretScan, OpenPR, Review, Fix, AwaitChecks, AwaitApproval, Merge}
}

// WaitKinds are the kinds no worker runs: an outside event completes them.
func WaitKinds() []string { return []string{AwaitChecks, AwaitApproval} }

// IsWait says whether kind is completed by an event rather than a worker.
func IsWait(kind string) bool { return slices.Contains(WaitKinds(), kind) }

// needsSandbox says whether kind runs in the chain's working tree.
func needsSandbox(kind string) bool { return !IsWait(kind) && kind != Merge }

// Item is what the planner needs from a work item.
type Item struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Repos []string `json:"repos"`
}

// RepoConfig is one repository's adjustments to the template. Start from
// DefaultRepoConfig: the zero value means no review and no merge.
type RepoConfig struct {
	// ReviewRounds is how many review rounds to unroll; 0 skips review, and
	// 1 reviews once with no fix round.
	ReviewRounds int `json:"review_rounds"`
	// RequireApproval waits for a human approval before merging.
	RequireApproval bool `json:"require_approval"`
	// Merge merges (or enqueues) the pull request; off leaves it for a person.
	Merge bool `json:"merge"`
	// After names repositories of the same item whose chains must finish
	// before this one starts.
	After []string `json:"after"`
	// Image is the dev container image the chain's sandbox runs in, a full
	// image reference; empty means the launcher's default. The planner
	// records it on the chain's workspace step and does not check it.
	Image string `json:"image,omitempty"`
}

// DefaultRepoConfig is the configuration of a repository that adjusts nothing.
func DefaultRepoConfig() RepoConfig { return RepoConfig{ReviewRounds: 3, Merge: true} }

// Template is the process: the step kinds of one repository's chain, in
// order. Review and await_checks expand into rounds with fix steps between
// them; review, await_approval and merge follow the repository's config.
type Template struct {
	Name  string   `json:"name"`
	Chain []string `json:"chain"`
}

// DefaultTemplate is the process that ships with the factory.
func DefaultTemplate() Template {
	return Template{Name: "default", Chain: []string{Workspace, Implement, Gate, SecretScan, OpenPR, Review, AwaitChecks, AwaitApproval, Merge}}
}

// Step is one node of a graph.
type Step struct {
	// Key is stable and readable: <repo>/<kind>, or <repo>/<kind>-<round>.
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params"`
	// SandboxKey groups the steps that share a working tree and so never run
	// at the same time. Empty for steps that need no sandbox.
	SandboxKey string `json:"sandbox_key"`
	// Condition, when set, is "<key> == <verdict>": the step runs only if that
	// step succeeded with that verdict, and is skipped otherwise.
	Condition string   `json:"condition,omitempty"`
	DependsOn []string `json:"depends_on"`
}

// Graph is a plan: the steps and, through DependsOn, the edges.
type Graph struct {
	Template   string `json:"template"`
	InputsHash string `json:"inputs_hash"`
	Steps      []Step `json:"steps"`
}

// Plan builds item's graph from the template and each repository's config.
// A repository missing from configs gets DefaultRepoConfig. Each repository
// gets its own chain; chains are independent unless a config's After orders
// them.
func Plan(item Item, configs map[string]RepoConfig, t Template) (Graph, error) {
	if len(item.Repos) == 0 {
		return Graph{}, fmt.Errorf("%w: the item names no repositories", ErrInvalidGraph)
	}
	resolved := make(map[string]RepoConfig, len(item.Repos))
	for _, repo := range item.Repos {
		cfg, ok := configs[repo]
		if !ok {
			cfg = DefaultRepoConfig()
		}
		if cfg.ReviewRounds < 0 {
			return Graph{}, fmt.Errorf("%w: %s: review_rounds must not be negative", ErrInvalidGraph, repo)
		}
		for _, a := range cfg.After {
			if a == repo || !slices.Contains(item.Repos, a) {
				return Graph{}, fmt.Errorf("%w: %s: after names %q, which is not another repository of the item", ErrInvalidGraph, repo, a)
			}
		}
		resolved[repo] = cfg
	}
	for _, kind := range t.Chain {
		if kind == Fix || !slices.Contains(Kinds(), kind) {
			return Graph{}, fmt.Errorf("%w: template %s: %q cannot be in a chain", ErrInvalidGraph, t.Name, kind)
		}
	}

	g := Graph{Template: t.Name, Steps: []Step{}}
	first := map[string]int{}   // repo -> index of its first step
	last := map[string]string{} // repo -> key of its last step
	for _, repo := range item.Repos {
		c := chain{g: &g, repo: repo, item: item.ID}
		first[repo] = len(g.Steps)
		cfg := resolved[repo]
		for _, kind := range t.Chain {
			switch {
			case kind == Review && cfg.ReviewRounds > 0:
				c.rounds(Review, cfg.ReviewRounds, "fix", VerdictChanges)
			case kind == AwaitChecks:
				c.rounds(AwaitChecks, checkRounds, "fix-checks", VerdictFail)
			case kind == Review, kind == AwaitApproval && !cfg.RequireApproval, kind == Merge && !cfg.Merge:
			default:
				params := map[string]any{"repo": repo}
				if kind == Workspace {
					params["branch"] = BranchName(item.ID, item.Title)
					if cfg.Image != "" {
						params["image"] = cfg.Image
					}
				}
				c.add(kind, kind, params, "")
			}
		}
		last[repo] = c.prev
	}
	for _, repo := range item.Repos {
		if len(g.Steps) == first[repo] {
			continue
		}
		for _, a := range resolved[repo].After {
			s := &g.Steps[first[repo]]
			s.DependsOn = append(s.DependsOn, last[a])
		}
	}
	if err := g.Validate(); err != nil {
		return Graph{}, err
	}
	hash, err := inputsHash(item, resolved, t)
	if err != nil {
		return Graph{}, err
	}
	g.InputsHash = hash
	return g, nil
}

// chain appends one repository's steps, each depending on the one before.
type chain struct {
	g    *Graph
	repo string
	item string
	prev string
}

func (c *chain) add(kind, name string, params map[string]any, condition string) string {
	key := c.repo + "/" + name
	deps := []string{}
	if c.prev != "" {
		deps = append(deps, c.prev)
	}
	sandbox := ""
	if needsSandbox(kind) {
		sandbox = c.item + "/" + c.repo
	}
	c.g.Steps = append(c.g.Steps, Step{Key: key, Kind: kind, Params: params, SandboxKey: sandbox, Condition: condition, DependsOn: deps})
	c.prev = key
	return key
}

// rounds unrolls kind-1, fixName-1, kind-2, ... kind-n. Every step after the
// first runs only if the previous round's verdict was retry.
func (c *chain) rounds(kind string, n int, fixName, retry string) {
	var previous string
	for round := 1; round <= n; round++ {
		condition := ""
		if round > 1 {
			condition = previous + " == " + retry
			c.add(Fix, fixName+"-"+strconv.Itoa(round-1), map[string]any{"repo": c.repo, "round": round - 1, "findings": previous}, condition)
		}
		previous = c.add(kind, kind+"-"+strconv.Itoa(round), map[string]any{"repo": c.repo, "round": round, "last": round == n}, condition)
	}
}

func inputsHash(item Item, configs map[string]RepoConfig, t Template) (string, error) {
	raw, err := json.Marshal(struct {
		Item     Item                  `json:"item"`
		Configs  map[string]RepoConfig `json:"configs"`
		Template Template              `json:"template"`
	}{item, configs, t})
	if err != nil {
		return "", fmt.Errorf("hash plan inputs: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks the graph rules. The store calls it before writing a graph,
// so a stored graph is always acyclic.
func (g Graph) Validate() error {
	byKey := make(map[string]Step, len(g.Steps))
	for _, s := range g.Steps {
		if s.Key == "" {
			return fmt.Errorf("%w: a step has no key", ErrInvalidGraph)
		}
		if _, dup := byKey[s.Key]; dup {
			return fmt.Errorf("%w: duplicate key %s", ErrInvalidGraph, s.Key)
		}
		if !slices.Contains(Kinds(), s.Kind) {
			return fmt.Errorf("%w: %s has unknown kind %q", ErrInvalidGraph, s.Key, s.Kind)
		}
		byKey[s.Key] = s
	}
	for _, s := range g.Steps {
		for _, d := range s.DependsOn {
			if _, ok := byKey[d]; !ok || d == s.Key {
				return fmt.Errorf("%w: %s depends on %q, which is not another step", ErrInvalidGraph, s.Key, d)
			}
		}
	}
	// Depth-first search: a step met again while still on the path is a cycle.
	const visiting, visited = 1, 2
	marks := map[string]int{}
	var visit func(string) error
	visit = func(key string) error {
		switch marks[key] {
		case visiting:
			return fmt.Errorf("%w: cycle through %s", ErrInvalidGraph, key)
		case visited:
			return nil
		}
		marks[key] = visiting
		for _, d := range byKey[key].DependsOn {
			if err := visit(d); err != nil {
				return err
			}
		}
		marks[key] = visited
		return nil
	}
	for _, s := range g.Steps {
		if err := visit(s.Key); err != nil {
			return err
		}
	}
	for _, s := range g.Steps {
		if s.Condition == "" {
			continue
		}
		on, _, ok := strings.Cut(s.Condition, " == ")
		if !ok || !ancestor(byKey, s.Key, on) {
			return fmt.Errorf("%w: %s: condition %q must be \"<ancestor key> == <verdict>\"", ErrInvalidGraph, s.Key, s.Condition)
		}
	}
	return nil
}

func ancestor(byKey map[string]Step, of, candidate string) bool {
	for _, d := range byKey[of].DependsOn {
		if d == candidate || ancestor(byKey, d, candidate) {
			return true
		}
	}
	return false
}

// Status is a step as the scheduler sees it: its state and, once succeeded,
// the verdict in its result.
type Status struct {
	State   string
	Verdict string
}

// Next returns the pending steps that change state given status (keyed by
// step key): a step whose dependencies have all succeeded or been skipped
// becomes ready, or skipped when its condition does not hold. A skip can
// unblock further steps, so it repeats until nothing changes. A failed or
// canceled dependency leaves its dependents pending.
func Next(g Graph, status map[string]Status) map[string]string {
	current := make(map[string]Status, len(status))
	for k, v := range status {
		current[k] = v
	}
	changes := map[string]string{}
	for changed := true; changed; {
		changed = false
		for _, s := range g.Steps {
			if current[s.Key].State != Pending || !depsDone(s, current) {
				continue
			}
			next := Ready
			if on, verdict, ok := strings.Cut(s.Condition, " == "); ok {
				if c := current[on]; c.State != Succeeded || c.Verdict != verdict {
					next = Skipped
				}
			}
			current[s.Key] = Status{State: next}
			changes[s.Key] = next
			changed = true
		}
	}
	return changes
}

func depsDone(s Step, status map[string]Status) bool {
	for _, d := range s.DependsOn {
		if st := status[d].State; st != Succeeded && st != Skipped {
			return false
		}
	}
	return true
}

// RoundsExhausted says whether a step that succeeded with result should
// count as failed instead: it is the last round (params "last": true) and
// its verdict is not pass, so no fix round is left.
func RoundsExhausted(params, result json.RawMessage) bool {
	var p struct {
		Last bool `json:"last"`
	}
	var r struct {
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal(params, &p)
	_ = json.Unmarshal(result, &r)
	return p.Last && r.Verdict != "" && r.Verdict != VerdictPass
}

// Verdict reads the verdict from a step's result, or "" when there is none.
func Verdict(result json.RawMessage) string {
	var r struct {
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal(result, &r)
	return r.Verdict
}
