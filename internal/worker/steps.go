package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/repoconfig"
)

// Stages a step reports as it goes. Each report resets the step's stuck
// timer on the server.
const (
	stageWorktreeReady   = "worktree_ready"
	stageSessionRunning  = "session_running"
	stageSessionFinished = "session_finished"
	stageGatePassed      = "gate_passed"
	stagePROpened        = "pr_opened"
	stageReview          = "review"
)

// stepRun is one claimed step and what it was handed.
type stepRun struct {
	e      *executor
	claim  Claim
	repo   string
	params struct {
		Repo     string `json:"repo"`
		Branch   string `json:"branch"`
		Findings string `json:"findings"`
	}
	// tokens is the token source for the code host API; token is the same
	// source with the commit identity the server sends beside the token,
	// for git.
	tokens func(context.Context) (string, error)
	token  func(context.Context) (codehost.Token, error)
}

func newStepRun(e *executor, claim Claim) (*stepRun, error) {
	s := &stepRun{e: e, claim: claim}
	if err := json.Unmarshal(claim.Step.Params, &s.params); err != nil {
		return nil, fmt.Errorf("step %s: decode params: %w", claim.Step.Key, err)
	}
	if s.params.Repo == "" {
		return nil, fmt.Errorf("step %s names no repo in its params", claim.Step.Key)
	}
	s.repo = s.params.Repo
	source := e.tokens(claim, s.repo)
	s.tokens, s.token = source.Get, source.Token
	return s, nil
}

// root is the sandbox's directory under the work directory.
func (s *stepRun) root() (string, error) {
	key := filepath.FromSlash(s.claim.Step.SandboxKey)
	if key == "" || !filepath.IsLocal(key) {
		return "", fmt.Errorf("step %s has sandbox key %q, which is not a relative path", s.claim.Step.Key, s.claim.Step.SandboxKey)
	}
	return filepath.Join(s.e.cfg.WorkDir, key), nil
}

// spec is the checkout of this step's repository under root.
func (s *stepRun) spec(root, base, branch string) treeSpec {
	return treeSpec{
		root: root, url: s.url(), base: base, branch: branch, token: s.token, gitBin: s.e.git, sweep: s.e.sweep,
		wait: s.e.cfg.netWait, log: s.e.cfg.Log,
	}
}

func (s *stepRun) url() string {
	return strings.TrimSuffix(s.e.cfg.GitURL, "/") + "/" + s.repo + ".git"
}

// workspaceResult is the workspace step's result document.
type workspaceResult struct {
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Head   string `json:"head"`
}

// tree opens the sandbox's checkout on the branch the workspace step
// recorded, re-creating it from the pushed branch when the disk is empty.
func (s *stepRun) tree(ctx context.Context) (*tree, error) {
	key := s.repo + "/workspace"
	var ws workspaceResult
	if raw, ok := s.claim.Results[key]; !ok || json.Unmarshal(raw, &ws) != nil || ws.Branch == "" || ws.Base == "" {
		return nil, fmt.Errorf("the claim carries no result for %s; replan the item", key)
	}
	root, err := s.root()
	if err != nil {
		return nil, err
	}
	return openTree(ctx, s.spec(root, ws.Base, ws.Branch))
}

// prepare opens the checkout and reads the repository's config from its
// base branch.
func (s *stepRun) prepare(ctx context.Context) (*tree, repoconfig.Config, *outcome) {
	t, err := s.tree(ctx)
	if err != nil {
		o := retry(err)
		return nil, repoconfig.Config{}, &o
	}
	cfg, err := t.config(ctx)
	if err != nil {
		o := configFailure(err)
		return nil, repoconfig.Config{}, &o
	}
	return t, cfg, nil
}

// configFailure is final for a missing or invalid config, which only a
// change to the repository fixes.
func configFailure(err error) outcome {
	if errors.Is(err, repoconfig.ErrInvalid) || errors.Is(err, ErrNoConfig) {
		return final(err)
	}
	return retry(err)
}

// pushFailure is final for a push refused for a secret or a moved branch.
func pushFailure(err error) outcome {
	if errors.Is(err, ErrSecretFound) || errors.Is(err, ErrBranchMoved) {
		return final(err)
	}
	return retry(err)
}

// workspace clones the repository and checks out the planned branch: the
// remote branch when an earlier attempt pushed it, otherwise a new branch
// off the default branch. Its result names the branch and base every later
// step of the chain rebuilds its checkout from.
func (s *stepRun) workspace(ctx context.Context) outcome {
	if s.params.Branch == "" {
		return final(fmt.Errorf("step %s has no branch param; replan the item", s.claim.Step.Key))
	}
	root, err := s.root()
	if err != nil {
		return final(err)
	}
	base, err := defaultBranch(ctx, s.spec(root, "", ""))
	if err != nil {
		return retry(err)
	}
	t, err := openTree(ctx, s.spec(root, base, s.params.Branch))
	if err != nil {
		return retry(err)
	}
	// Fail now, before any session, when the repository is not set up.
	if _, err := t.config(ctx); err != nil {
		return configFailure(err)
	}
	head, err := t.head(ctx)
	if err != nil {
		return retry(err)
	}
	s.e.stage(ctx, s.claim, stageWorktreeReady)
	return succeeded(workspaceResult{Branch: t.branch, Base: t.base, Head: head})
}

// headResult is the result of a step that moves the branch.
type headResult struct {
	Head string `json:"head"`
}

// implement runs the writing session, commits its change with the pull
// request text it left, and pushes the branch so a later step in another
// sandbox finds it. A branch already ahead of its base with nothing
// uncommitted means an earlier attempt finished the session, so it is only
// pushed again.
func (s *stepRun) implement(ctx context.Context) outcome {
	t, cfg, failed := s.prepare(ctx)
	if failed != nil {
		return *failed
	}
	ahead, err := t.ahead(ctx)
	if err != nil {
		return retry(err)
	}
	clean, err := t.clean(ctx)
	if err != nil {
		return retry(err)
	}
	if ahead == 0 || !clean {
		s.e.stage(ctx, s.claim, stageSessionRunning)
		if err := s.e.session(ctx, t.dir, brief(s.claim.Item, s.repo, cfg.Gate), cfg.SessionTimeout); err != nil {
			return retry(err)
		}
		if question, ok := readNote(t.dir, blockedPath); ok {
			dropNotes(t.dir)
			return outcome{blocked: question}
		}
		title, body, _ := prText(t.dir, s.claim.Item)
		dropNotes(t.dir)
		changed, err := t.commit(ctx, strings.TrimSpace(title+"\n\n"+body))
		if err != nil {
			return retry(err)
		}
		if !changed {
			return retry(errors.New("the session made no changes"))
		}
	}
	if err := t.push(ctx); err != nil {
		return pushFailure(err)
	}
	head, err := t.head(ctx)
	if err != nil {
		return retry(err)
	}
	s.e.stage(ctx, s.claim, stageSessionFinished)
	return succeeded(headResult{Head: head})
}

// runGate runs the repository's gate and keeps its output with the step on
// the server, passed or failed. A failure is final; its reason leads with
// the lines that look like failures, then the end of the output, and says
// where the full output is. output is what was kept, for a session to read.
func (s *stepRun) runGate(ctx context.Context, t *tree, cfg repoconfig.Config) (output []byte, failed *outcome) {
	out, err := t.runGate(ctx, cfg.Gate, cfg.GateTimeout)
	if ctx.Err() != nil {
		o := retry(ctx.Err())
		return nil, &o
	}
	output = out.Bytes()
	kept := s.keepOutput(ctx, output, out.Total())
	if err == nil {
		return output, nil
	}
	o := final(errors.New(gateFailure(cfg.Gate, err, output, kept)))
	return output, &o
}

// keepOutput uploads a gate run's output and returns the sentence a
// failure reason ends with: where the full output is, or why it was not
// kept. A failed upload does not fail the step.
func (s *stepRun) keepOutput(ctx context.Context, output []byte, total int64) string {
	if err := s.e.cfg.Client.UploadOutput(ctx, s.claim.Step.ID, s.e.workerID, output); err != nil {
		s.e.cfg.Log.Warn("could not keep the gate output", "step_id", s.claim.Step.ID, "err", err)
		return fmt.Sprintf("The full output (%d bytes) could not be kept: %v", total, err)
	}
	size := fmt.Sprintf("%d bytes", total)
	if total > int64(len(output)) {
		size = fmt.Sprintf("%d bytes, cut to its first and last lines", total)
	}
	return fmt.Sprintf("The full output (%s) is kept with the step: GET /api/v1/steps/%s/output, or the step's Full gate output link on the work item's page.", size, s.claim.Step.ID)
}

// gateResult is a gate round's result. failure is the failed gate's
// reason, which the fix-gate session after it is handed.
type gateResult struct {
	Verdict string `json:"verdict"`
	Failure string `json:"failure,omitempty"`
}

// gate runs the repository's check command on the branch. It only reads.
// A failed round with a fix round left after it succeeds with the verdict
// fail; on the last round, or in a graph planned without gate rounds, the
// failure is final.
func (s *stepRun) gate(ctx context.Context) outcome {
	var p struct {
		Round int  `json:"round"`
		Last  bool `json:"last"`
	}
	_ = json.Unmarshal(s.claim.Step.Params, &p)
	t, cfg, failed := s.prepare(ctx)
	if failed != nil {
		return *failed
	}
	if _, failed := s.runGate(ctx, t, cfg); failed != nil {
		switch {
		case failed.retryable || p.Round == 0:
			return *failed
		case p.Last:
			return final(fmt.Errorf("the gate failed on all %d runs, with a session trying to fix it after each but the last; the last run: %w", p.Round, failed.err))
		}
		return succeeded(gateResult{Verdict: "fail", Failure: failed.err.Error()})
	}
	s.e.stage(ctx, s.claim, stageGatePassed)
	return succeeded(gateResult{Verdict: "pass"})
}

// gateFailure is the failure of the gate round a fix step's params name,
// or "" when that step is not a failed gate round.
func (s *stepRun) gateFailure() string {
	var r gateResult
	_ = json.Unmarshal(s.claim.Results[s.params.Findings], &r)
	return r.Failure
}

// fixGate runs a session on a failed gate round, refuses a change to the
// gate, and commits and pushes the rest. It does not run the gate: the next
// round does. A commit for this round already on the branch means an
// earlier attempt finished the session, so it is only pushed again.
func (s *stepRun) fixGate(ctx context.Context, failure string) outcome {
	t, cfg, failed := s.prepare(ctx)
	if failed != nil {
		return *failed
	}
	subject := "Make the gate pass after " + s.params.Findings
	done, err := t.hasSubject(ctx, subject)
	if err != nil {
		return retry(err)
	}
	if !done {
		s.e.stage(ctx, s.claim, stageSessionRunning)
		if err := s.e.session(ctx, t.dir, fixGateBrief(s.claim.Item, s.repo, cfg.Gate, failure), cfg.SessionTimeout); err != nil {
			return retry(err)
		}
		if question, ok := readNote(t.dir, blockedPath); ok {
			dropNotes(t.dir)
			return outcome{blocked: question}
		}
		dropNotes(t.dir)
		if o := s.refuseGateChange(ctx, t, cfg); o != nil {
			return *o
		}
		clean, err := t.clean(ctx)
		if err != nil {
			return retry(err)
		}
		if clean {
			return retry(errors.New("the session made no changes for the failed gate"))
		}
		if _, err := t.commit(ctx, subject); err != nil {
			return retry(err)
		}
	}
	if err := t.push(ctx); err != nil {
		return pushFailure(err)
	}
	head, err := t.head(ctx)
	if err != nil {
		return retry(err)
	}
	s.e.stage(ctx, s.claim, stageSessionFinished)
	return succeeded(headResult{Head: head})
}

// refuseGateChange fails the step, for good, when a session that was asked
// to make the gate pass changed or deleted a file that defines the gate:
// the repository's config or a file the gate command names. A new file is
// not a change to the gate. It returns nil when there was none.
func (s *stepRun) refuseGateChange(ctx context.Context, t *tree, cfg repoconfig.Config) *outcome {
	changed, err := t.changed(ctx)
	if err != nil {
		o := retry(err)
		return &o
	}
	gateFiles := gateFiles(cfg.Gate)
	for _, path := range changed {
		if slices.Contains(gateFiles, path) {
			o := final(fmt.Errorf("the session asked to make the gate pass changed %s, which defines the gate; a session may not change the gate to pass it, so a person must look at the branch", path))
			return &o
		}
	}
	return nil
}

// gateFiles are the paths that define a gate command: the repository's
// config, each word of the command that reads as a relative path, and the
// root makefiles when the command runs make. Words are not checked against
// the tree, so an unusual word only widens what a session may not change.
func gateFiles(command string) []string {
	files := []string{repoconfig.Path}
	words := strings.FieldsFunc(command, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`;&|()'"`+"`", r) })
	for _, w := range words {
		if w == "make" {
			files = append(files, "Makefile", "makefile", "GNUmakefile")
			continue
		}
		if p := path.Clean(w); filepath.IsLocal(p) && !strings.HasPrefix(w, "-") {
			files = append(files, p)
		}
	}
	return files
}

// secretScan scans the lines the branch adds for credentials. It only reads.
func (s *stepRun) secretScan(ctx context.Context) outcome {
	t, err := s.tree(ctx)
	if err != nil {
		return retry(err)
	}
	if _, err := t.scanSecrets(ctx); err != nil {
		return pushFailure(err)
	}
	return succeeded(struct{}{})
}

// pull is the newest pull request output the item has for the step's
// repository.
func (s *stepRun) pull() (Output, bool) {
	for i := len(s.claim.Outputs) - 1; i >= 0; i-- {
		o := s.claim.Outputs[i]
		if o.Kind == "pull_request" && strings.EqualFold(o.Repo, s.repo) {
			return o, true
		}
	}
	return Output{}, false
}

// pullBody is the description of this repository's pull request for the
// item, with summary first.
func (s *stepRun) pullBody(summary string) string {
	var siblings []string
	for _, o := range s.claim.Outputs {
		if o.Kind == "pull_request" && !strings.EqualFold(o.Repo, s.repo) {
			siblings = append(siblings, o.URL)
		}
	}
	return codehost.PullBody(s.repo, summary, s.claim.Item.ExternalURL, siblings)
}

// updatePull rewrites the title and body of the recorded pull request.
func (s *stepRun) updatePull(ctx context.Context, title, body string) error {
	o, ok := s.pull()
	if !ok {
		return fmt.Errorf("no pull request is recorded for %s, so its text cannot be updated", s.repo)
	}
	n, err := codehost.PullNumber(o.URL)
	if err != nil {
		return err
	}
	return s.e.cfg.Host(s.repo, s.tokens).UpdatePull(ctx, n, title, s.pullBody(body))
}

// urlResult is open_pr's result.
type urlResult struct {
	URL string `json:"url"`
}

// openPR pushes the branch and opens its pull request, recording it as an
// output. A pull request already recorded is the result; one opened by an
// attempt that died before recording it is found by its branch.
func (s *stepRun) openPR(ctx context.Context) outcome {
	if o, ok := s.pull(); ok {
		return succeeded(urlResult{URL: o.URL})
	}
	t, err := s.tree(ctx)
	if err != nil {
		return retry(err)
	}
	dropNotes(t.dir)
	changed, err := t.commit(ctx, s.claim.Item.Title)
	if err != nil {
		return retry(err)
	}
	if !changed {
		return final(fmt.Errorf("%s has no commits after %s, so there is nothing to open a pull request for", t.branch, t.base))
	}
	if err := t.push(ctx); err != nil {
		return pushFailure(err)
	}
	host := s.e.cfg.Host(s.repo, s.tokens)
	pull, found, err := host.FindPull(ctx, t.branch, "all")
	if err != nil {
		return retry(err)
	}
	if !found {
		title, body, err := t.firstMessage(ctx)
		if err != nil {
			return retry(err)
		}
		if title == "" {
			title = s.claim.Item.Title
		}
		pull, err = host.OpenPull(ctx, title, s.pullBody(body), t.branch, t.base)
		if err != nil {
			return retry(err)
		}
	}
	status := "open"
	switch {
	case pull.MergedAt != "":
		status = "merged"
	case pull.State == "closed":
		status = "closed"
	}
	out := Output{Repo: s.repo, Kind: "pull_request", Branch: t.branch, URL: pull.HTMLURL, Status: status}
	// Recorded at once, so a crash before the result still leaves it.
	s.e.stage(ctx, s.claim, stagePROpened, out)
	return succeeded(urlResult{URL: pull.HTMLURL}, out)
}

// reviewResult is review's result.
type reviewResult struct {
	Verdict  string `json:"verdict"`
	Findings string `json:"findings"`
}

// review runs a review session on the branch and reports its verdict. It
// changes nothing: anything the session left is discarded.
func (s *stepRun) review(ctx context.Context) outcome {
	t, cfg, failed := s.prepare(ctx)
	if failed != nil {
		return *failed
	}
	s.e.stage(ctx, s.claim, stageReview)
	err := s.e.session(ctx, t.dir, reviewBrief(s.claim.Item, s.repo), cfg.SessionTimeout)
	verdict, findings, verdictErr := readVerdict(t.dir)
	if discardErr := t.discard(ctx); discardErr != nil && err == nil {
		err = discardErr
	}
	if err == nil {
		err = verdictErr
	}
	if err != nil {
		return retry(err)
	}
	return succeeded(reviewResult{Verdict: verdict, Findings: findings})
}

// fix runs a session on the findings of the review its params name, gates
// the change, and pushes it, which updates the open pull request. When the
// session rewrites the pull request text, the open pull request's title and
// body are updated from it; that alone is a change, with nothing to commit,
// gate or push. A commit
// for this round already on the branch means an earlier attempt finished the
// session, so it is gated and pushed again. When the gate fails, a second
// session is handed the gate's output to make it pass, and the gate runs
// again; a second failure is final.
func (s *stepRun) fix(ctx context.Context) outcome {
	// A review's findings, or the failed checks of an await_checks round.
	var r struct {
		Findings     string          `json:"findings"`
		FailedChecks json.RawMessage `json:"failed_checks"`
	}
	if raw, ok := s.claim.Results[s.params.Findings]; ok && json.Unmarshal(raw, &r) == nil && r.Findings == "" && len(r.FailedChecks) > 0 {
		r.Findings = "These checks failed on the pull request: " + string(r.FailedChecks)
	}
	if strings.TrimSpace(r.Findings) == "" {
		return final(fmt.Errorf("the claim carries no findings from %q; replan the item", s.params.Findings))
	}
	t, cfg, failed := s.prepare(ctx)
	if failed != nil {
		return *failed
	}
	// One subject per findings step, so fix-1 and fix-checks-1 differ.
	subject := "Address the findings of " + s.params.Findings
	done, err := t.hasSubject(ctx, subject)
	if err != nil {
		return retry(err)
	}
	if !done {
		s.e.stage(ctx, s.claim, stageSessionRunning)
		if err := s.e.session(ctx, t.dir, fixBrief(s.claim.Item, s.repo, cfg.Gate, r.Findings), cfg.SessionTimeout); err != nil {
			return retry(err)
		}
		if question, ok := readNote(t.dir, blockedPath); ok {
			dropNotes(t.dir)
			return outcome{blocked: question}
		}
		title, body, rewrote := prText(t.dir, s.claim.Item)
		dropNotes(t.dir)
		if rewrote {
			if err := s.updatePull(ctx, title, body); err != nil {
				return retry(err)
			}
		}
		clean, err := t.clean(ctx)
		if err != nil {
			return retry(err)
		}
		if clean && !rewrote {
			return retry(errors.New("the session made no changes for the review findings"))
		}
		if clean {
			head, err := t.head(ctx)
			if err != nil {
				return retry(err)
			}
			s.e.stage(ctx, s.claim, stageSessionFinished)
			return succeeded(headResult{Head: head})
		}
		if _, err := t.commit(ctx, subject); err != nil {
			return retry(err)
		}
	}
	if output, failed := s.runGate(ctx, t, cfg); failed != nil {
		if failed.retryable {
			return *failed
		}
		// The session gets one more go, with its gate's output.
		if o := s.repairGate(ctx, t, cfg, output, failed.err.Error(), subject); o != nil {
			return *o
		}
		if _, failed := s.runGate(ctx, t, cfg); failed != nil {
			return *failed
		}
	}
	if err := t.push(ctx); err != nil {
		return pushFailure(err)
	}
	head, err := t.head(ctx)
	if err != nil {
		return retry(err)
	}
	s.e.stage(ctx, s.claim, stageSessionFinished)
	return succeeded(headResult{Head: head})
}

// repairGate runs a session on a failed gate: the reason inline, the whole
// output kept in gateOutputPath, and commits what it changed. It returns
// nil when there is a change to gate again, and otherwise the outcome:
// blocked, a retryable session failure, or the gate's failure when the
// session changed nothing.
func (s *stepRun) repairGate(ctx context.Context, t *tree, cfg repoconfig.Config, output []byte, reason, subject string) *outcome {
	gateFailed := final(errors.New(reason))
	file := filepath.Join(t.dir, filepath.FromSlash(gateOutputPath))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		o := retry(fmt.Errorf("write the gate output for the session: %w", err))
		return &o
	}
	if err := os.WriteFile(file, output, 0o600); err != nil {
		o := retry(fmt.Errorf("write the gate output for the session: %w", err))
		return &o
	}
	s.e.stage(ctx, s.claim, stageSessionRunning)
	if err := s.e.session(ctx, t.dir, gateFixBrief(s.claim.Item, s.repo, cfg.Gate, reason), cfg.SessionTimeout); err != nil {
		dropNotes(t.dir)
		o := retry(err)
		return &o
	}
	if question, ok := readNote(t.dir, blockedPath); ok {
		dropNotes(t.dir)
		return &outcome{blocked: question}
	}
	dropNotes(t.dir)
	if o := s.refuseGateChange(ctx, t, cfg); o != nil {
		return o
	}
	clean, err := t.clean(ctx)
	if err != nil {
		o := retry(err)
		return &o
	}
	if clean {
		return &gateFailed
	}
	if _, err := t.commit(ctx, "Make the gate pass: "+subject); err != nil {
		o := retry(err)
		return &o
	}
	return nil
}

// mergeResult is merge's result.
type mergeResult struct {
	Merged bool `json:"merged"`
	Queued bool `json:"queued"`
}

// merge merges the recorded pull request, or adds it to the merge queue when
// its base has one, and records it merged. A merged pull request is not
// merged again.
func (s *stepRun) merge(ctx context.Context) outcome {
	o, ok := s.pull()
	if !ok {
		return final(fmt.Errorf("no pull request is recorded for %s, so there is nothing to merge", s.repo))
	}
	if o.Status == "merged" {
		return succeeded(mergeResult{Merged: true})
	}
	n, err := codehost.PullNumber(o.URL)
	if err != nil {
		return final(err)
	}
	host := s.e.cfg.Host(s.repo, s.tokens)
	merged := o
	merged.Status = "merged"
	if o.Branch != "" {
		pull, found, err := host.FindPull(ctx, o.Branch, "all")
		if err != nil {
			return retry(err)
		}
		if found && pull.Number == n && pull.MergedAt != "" {
			return succeeded(mergeResult{Merged: true}, merged)
		}
	}
	queue, err := host.QueuedPull(ctx, n)
	if err != nil {
		return retry(err)
	}
	if queue != "" {
		if err := host.EnqueuePull(ctx, queue); err != nil {
			return retry(err)
		}
		return succeeded(mergeResult{Queued: true})
	}
	if err := host.MergePull(ctx, n); err != nil {
		return retry(err)
	}
	return succeeded(mergeResult{Merged: true}, merged)
}
