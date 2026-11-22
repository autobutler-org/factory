package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

// poller is the actor of every change a poll makes.
var poller = work.Actor{Kind: work.ActorSystem, ID: "poller"}

// poll reads every open pull request of the items being worked from the
// code host, one request each, and acts on what it sees. It returns how
// many changes it made. A pull request the code host cannot answer for is
// logged and left for the next poll; a database error ends the poll.
func (l *Launcher) poll(ctx context.Context) (int, error) {
	rows, err := l.Work.PollPulls(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, row := range rows {
		st, err := l.pullStatus(ctx, row)
		if err != nil {
			l.Logger.Warn("could not read a pull request; the next poll tries again", "work_item_id", row.WorkItemID, "url", row.Url, "err", err)
			continue
		}
		ok, err := l.completeWait(ctx, row, st)
		if err != nil {
			return changed, fmt.Errorf("poll %s: %w", row.Url, err)
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

func (l *Launcher) pullStatus(ctx context.Context, row db.PollPullsRow) (codehost.PullStatus, error) {
	number, err := codehost.PullNumber(row.Url)
	if err != nil {
		return codehost.PullStatus{}, err
	}
	host, err := l.Host(ctx, row.Repo)
	if err != nil {
		return codehost.PullStatus{}, fmt.Errorf("code host for %s: %w", row.Repo, err)
	}
	return host.PullStatus(ctx, number)
}

// completeWait completes the ready wait step beside an open pull request
// when what it waits for has happened, and reports whether it did. A step
// someone else completed first is left alone.
func (l *Launcher) completeWait(ctx context.Context, row db.PollPullsRow, st codehost.PullStatus) (bool, error) {
	if row.StepID == nil || st.State != codehost.PullOpen {
		return false, nil
	}
	var result any
	switch *row.StepKind {
	case plan.AwaitChecks:
		since := st.HeadCommitted
		if row.ReadySince != nil && row.ReadySince.After(since) {
			since = *row.ReadySince
		}
		v := checksVerdict(st, since, l.ChecksGrace, time.Now())
		if v.Verdict == "" {
			return false, nil
		}
		result = v
	case plan.AwaitApproval:
		if !st.Approved {
			return false, nil
		}
		result = map[string]any{"approved": true, "head": st.Head}
	default:
		return false, nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	_, err = l.Work.CompleteWait(ctx, *row.StepID, work.StepResult{Outcome: work.StepSucceeded, Result: raw}, poller)
	if errors.Is(err, work.ErrNotWaiting) {
		return false, nil
	}
	return err == nil, err
}

// checksResult is an await_checks result: the verdict, the failed checks
// the fix round reads, and what the poller counted, on which head.
type checksResult struct {
	Verdict      string           `json:"verdict"`
	FailedChecks []string         `json:"failed_checks,omitempty"`
	Head         string           `json:"head"`
	Required     []string         `json:"required,omitempty"`
	Checks       []codehost.Check `json:"checks"`
}

// checksVerdict decides an await_checks step from the head commit's checks:
// only the required ones when the base branch names some (one not reported
// yet is pending), otherwise every one. Any failure fails at once; any
// pending check waits. A head with no checks at all passes once grace has
// gone by since since. An empty Verdict means wait.
func checksVerdict(st codehost.PullStatus, since time.Time, grace time.Duration, now time.Time) checksResult {
	r := checksResult{Head: st.Head, Required: st.Required, Checks: st.Checks}
	if len(st.Required) > 0 {
		r.Checks = make([]codehost.Check, 0, len(st.Required))
		for _, name := range st.Required {
			r.Checks = append(r.Checks, requiredCheck(st.Checks, name))
		}
	}
	if r.Checks == nil {
		r.Checks = []codehost.Check{}
	}
	pending := false
	for _, c := range r.Checks {
		switch c.State {
		case codehost.CheckFailed:
			r.FailedChecks = append(r.FailedChecks, c.Name)
		case codehost.CheckPending:
			pending = true
		}
	}
	switch {
	case len(r.FailedChecks) > 0:
		r.Verdict = "fail"
	case pending:
	case len(r.Checks) == 0 && now.Before(since.Add(grace)):
	default:
		r.Verdict = "pass"
	}
	return r
}

// requiredCheck folds the checks reported under name: failed if any failed,
// else pending if any is pending or none is reported, else passed.
func requiredCheck(checks []codehost.Check, name string) codehost.Check {
	state := ""
	for _, c := range checks {
		switch {
		case c.Name != name:
		case c.State == codehost.CheckFailed:
			return c
		case state != codehost.CheckPending:
			state = c.State
		}
	}
	if state == "" {
		state = codehost.CheckPending
	}
	return codehost.Check{Name: name, State: state}
}
