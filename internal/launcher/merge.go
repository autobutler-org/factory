package launcher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

// serverWorker is the worker name the server claims server-side steps as.
// Every replica shares it; only the replica that claimed a step reports it.
const serverWorker = "factory-server"

// maxServerSteps bounds the server-side steps one pass runs, so a backlog
// cannot hold the pass for long.
const maxServerSteps = 20

// runServerSteps claims and runs server-side steps until none is left or
// the bound is reached, and returns how many it reported.
func (l *Launcher) runServerSteps(ctx context.Context) (int, error) {
	w, err := l.Work.RegisterWorker(ctx, work.WorkerInput{Name: serverWorker, Kind: work.WorkerCluster})
	if err != nil {
		return 0, err
	}
	for n := range maxServerSteps {
		c, ok, err := l.Work.ClaimServerStep(ctx, w.ID)
		if err != nil || !ok {
			return n, err
		}
		if _, err := l.Work.ReportStepResult(ctx, c.Step.ID, w.ID, l.runStep(ctx, c)); err != nil {
			return n, fmt.Errorf("report %s: %w", c.Step.Key, err)
		}
	}
	return maxServerSteps, nil
}

func (l *Launcher) runStep(ctx context.Context, c work.StepClaim) work.StepResult {
	if c.Step.Kind != plan.Merge {
		return failed(false, "step kind %s cannot run in the server", c.Step.Kind)
	}
	var p struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(c.Step.Params, &p); err != nil || p.Repo == "" {
		return failed(false, "merge step %s names no repo", c.Step.Key)
	}
	return l.merge(ctx, p.Repo, c.Outputs)
}

// merge merges the item's pull request for repo, or adds it to the merge
// queue when its base branch has one. It is safe to run twice: a pull
// request already merged, recorded or found on the code host, succeeds
// without merging again.
func (l *Launcher) merge(ctx context.Context, repo string, outputs []db.WorkOutput) work.StepResult {
	var pr *db.WorkOutput
	for k := len(outputs) - 1; k >= 0; k-- {
		if outputs[k].Repo == repo && outputs[k].Kind == work.KindPullRequest {
			pr = &outputs[k]
			break
		}
	}
	switch {
	case pr == nil:
		return failed(false, "no pull request is recorded for %s", repo)
	case pr.Status == work.OutputMerged:
		return mergedResult(nil)
	case pr.Status == work.OutputClosed:
		return failed(false, "pull request %s was closed without merging", pr.Url)
	}
	number, err := codehost.PullNumber(pr.Url)
	if err != nil {
		return failed(false, "%v", err)
	}
	host, err := l.Host(ctx, repo)
	if err != nil {
		return failed(true, "code host for %s: %v", repo, err)
	}
	landed := work.OutputInput{Repo: repo, Kind: work.KindPullRequest, Branch: pr.Branch, URL: pr.Url, Status: work.OutputMerged}
	if pr.Branch != "" {
		found, ok, err := host.FindPull(ctx, pr.Branch, "all")
		if err != nil {
			return failed(true, "find pull request %s: %v", pr.Url, err)
		}
		switch {
		case ok && found.Number == number && found.MergedAt != "":
			return mergedResult(&landed)
		case ok && found.Number == number && found.State == "closed":
			return failed(false, "pull request %s was closed without merging", pr.Url)
		}
	}
	queue, err := host.QueuedPull(ctx, number)
	if err != nil {
		return failed(true, "read the merge queue of %s: %v", pr.Url, err)
	}
	if queue != "" {
		if err := host.EnqueuePull(ctx, queue); err != nil {
			return failed(true, "enqueue %s: %v", pr.Url, err)
		}
		// The poll records the output merged when the queue lands it.
		return work.StepResult{Outcome: work.StepSucceeded, Result: json.RawMessage(`{"merged":false,"queued":true}`)}
	}
	if err := host.MergePull(ctx, number); err != nil {
		return failed(true, "merge %s: %v", pr.Url, err)
	}
	return mergedResult(&landed)
}

func mergedResult(landed *work.OutputInput) work.StepResult {
	r := work.StepResult{Outcome: work.StepSucceeded, Result: json.RawMessage(`{"merged":true,"queued":false}`)}
	if landed != nil {
		r.Outputs = []work.OutputInput{*landed}
	}
	return r
}

func failed(retryable bool, format string, args ...any) work.StepResult {
	return work.StepResult{Outcome: work.StepFailed, Reason: fmt.Sprintf(format, args...), Retryable: retryable, Result: json.RawMessage("null")}
}
