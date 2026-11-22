// Package worker is the step executor that runs inside a worker sandbox: it
// registers with the factory server, heartbeats, claims one step at a time,
// runs the code for the step's kind, and reports the result, until the
// server has nothing more to hand it. See docs/architecture/worker.md.
//
// Each sandbox step kind (workspace, implement, gate, secret_scan, open_pr,
// review, fix) and merge is ported from schmeide's runner, re-pointed at the
// step protocol. Every kind is safe to run twice and rebuilds what it needs
// from the claim (the results of earlier steps and the item's outputs) and
// the pushed branch, so a step can land in a fresh sandbox with an empty
// disk. Each gate run's output, cut to 5 MiB, is uploaded to the server so it
// outlives the sandbox, and a failed gate's reason quotes the lines that look
// like failures; a fix whose gate fails hands that output to one more
// session.
//
// Sessions and the gate run as the worker's children, as the same user, so
// the worker keeps them away from its credentials itself: it is
// non-dumpable, and with Config.Contained it checks that at startup and
// kills every other process before git gets a code host token; git with a
// token only runs in a clone reset to state the worker wrote (the session
// boundary in docs/architecture/worker.md).
//
// The package does not decide what runs next (the server's graph does),
// does not plan, and does not start sandboxes. cmd/factory parses the flags
// and calls Run.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/harness"
)

const (
	// releaseTimeout bounds the release of a step on shutdown.
	releaseTimeout = 10 * time.Second
	// checkTimeout bounds the startup check that a child cannot read the
	// worker.
	checkTimeout = 10 * time.Second
)

// errLost is the cancel cause of a step the heartbeat no longer lists.
var errLost = errors.New("the heartbeat no longer lists the step")

// Config is what a worker needs.
type Config struct {
	// Client talks to the factory server.
	Client *Client
	// Name is the worker's name; the server gives the same name the same ID.
	Name string
	// Version is reported when the worker registers.
	Version string
	// SandboxKey, when set, restricts claims to that chain's steps.
	SandboxKey string
	// WorkDir holds one checkout per sandbox key.
	WorkDir string
	// Harnesses run agent sessions, in order of preference.
	Harnesses []harness.Harness
	// GitURL is the base of a repository's clone URL:
	// <GitURL>/<owner>/<name>.git.
	GitURL string
	// Host returns the code host for repo, asking tokens for its token.
	Host func(repo string, tokens func(context.Context) (string, error)) codehost.Host
	// IdleLimit kills a session silent this long. Zero is DefaultIdleLimit.
	IdleLimit time.Duration
	// Output receives session output. Nil discards it.
	Output io.Writer
	// Log is the worker's logger. Nil discards.
	Log *slog.Logger
	// Contained says the worker has a container of its own (a sandbox Job):
	// Run then refuses to start unless a child cannot read its memory, and
	// kills every other process in the container before any git command
	// that carries a code host token. Never set it on a shared machine:
	// that kill reaches every process of the user.
	Contained bool
}

// current is the step being worked on, shared with the heartbeat.
type current struct {
	mu     sync.Mutex
	id     string
	cancel context.CancelCauseFunc
}

func (c *current) set(id string, cancel context.CancelCauseFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id, c.cancel = id, cancel
}

// drop cancels the current step when held does not list it.
func (c *current) drop(held []string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id == "" || slices.Contains(held, c.id) {
		return "", false
	}
	c.cancel(errLost)
	return c.id, true
}

// Run registers, then claims and runs steps until the server has none left
// for this worker, and returns nil. When ctx ends (SIGTERM), the step in hand
// is released, which counts no attempt, and Run returns nil. A step whose
// claim is lost is abandoned without a report.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.IdleLimit <= 0 {
		cfg.IdleLimit = DefaultIdleLimit
	}
	if len(cfg.Harnesses) == 0 {
		return errors.New("no agent harness is configured; install claude or grok, or pass --harnesses")
	}
	// Before anything else runs: no session may read the worker's memory,
	// where its credential and code host tokens live (#48).
	if err := protect(); err != nil {
		return err
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git was not found on PATH (%w); install git", err)
	}
	if gitBin, err = filepath.Abs(gitBin); err != nil {
		return fmt.Errorf("resolve git: %w", err)
	}
	var sweepFn func(context.Context) error
	if cfg.Contained {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := checkProtected(checkCtx)
		cancel()
		if err == nil {
			err = checkGit(gitBin)
		}
		if err != nil {
			return fmt.Errorf("the session boundary does not hold, so no session is started: %w. "+
				"Run the worker on a runtime that honors non-dumpable processes (docs/architecture/worker.md)", err)
		}
		sweepFn = sweep
	}
	names := make([]string, len(cfg.Harnesses))
	for i, h := range cfg.Harnesses {
		names[i] = h.Name()
	}
	id, credential, err := cfg.Client.Register(ctx, cfg.Name, cfg.Version, names)
	if err != nil {
		return err
	}
	if credential != "" {
		// From here on the worker is its credential, held only in memory; the
		// pod's token is spent.
		client := *cfg.Client
		client.Token = func() (string, error) { return credential, nil }
		cfg.Client = &client
	}
	_, lease, err := cfg.Client.Heartbeat(ctx, id)
	if err != nil {
		return err
	}
	log := cfg.Log.With("worker_id", id)
	log.Info("registered", "name", cfg.Name, "sandbox_key", cfg.SandboxKey, "lease", lease)

	cur := &current{}
	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go heartbeat(beatCtx, cfg.Client, id, max(lease/3, time.Second), cur, log)

	e := &executor{cfg: cfg, workerID: id, exhausted: map[string]bool{}, git: gitBin, sweep: sweepFn}
	for {
		claim, ok, err := cfg.Client.Claim(ctx, id, cfg.SandboxKey)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return err
		case !ok:
			log.Info("nothing left to claim; exiting")
			return nil
		}
		sl := log.With("step_id", claim.Step.ID, "key", claim.Step.Key, "kind", claim.Step.Kind)
		sl.Info("claimed")
		stepCtx, cancel := context.WithCancelCause(ctx)
		cur.set(claim.Step.ID, cancel)
		out := e.run(stepCtx, claim)
		cause := context.Cause(stepCtx)
		cur.set("", nil)
		cancel(nil)
		switch {
		case errors.Is(cause, errLost):
			sl.Warn("lost the step's claim; not reporting on it")
			continue
		case ctx.Err() != nil:
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			err := cfg.Client.Release(rctx, claim.Step.ID, id, "worker shutting down")
			rcancel()
			if err != nil {
				sl.Error("could not release the step on shutdown; it returns when its lease expires", "err", err)
			} else {
				sl.Info("released the step on shutdown")
			}
			return nil
		}
		r := out.report()
		sl.Info("step finished", "outcome", r.Outcome, "reason", r.Reason, "retryable", r.Retryable)
		if err := cfg.Client.Report(ctx, claim.Step.ID, id, r); err != nil {
			if errors.Is(err, ErrLost) {
				sl.Warn("the claim ended before the result was reported", "err", err)
				continue
			}
			return err
		}
	}
}

// heartbeat extends the worker's leases every interval and cancels the
// current step when the server no longer lists it.
func heartbeat(ctx context.Context, c *Client, id string, interval time.Duration, cur *current, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		held, _, err := c.Heartbeat(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("heartbeat failed", "err", err)
			}
			continue
		}
		if step, dropped := cur.drop(held); dropped {
			log.Warn("the heartbeat no longer lists the step; stopping it", "step_id", step)
		}
	}
}

// outcome is how one run of a step ended.
type outcome struct {
	result  any
	outputs []Output
	// err is a failure; retryable says whether a later attempt could pass.
	err       error
	retryable bool
	// blocked, when set, is the decision the step needs.
	blocked string
}

func succeeded(result any, outputs ...Output) outcome {
	return outcome{result: result, outputs: outputs}
}

// retry is a failure a later attempt may not repeat.
func retry(err error) outcome { return outcome{err: err, retryable: true} }

// final is a failure a later attempt would repeat.
func final(err error) outcome { return outcome{err: err} }

func (o outcome) report() Result {
	switch {
	case o.blocked != "":
		return Result{Outcome: "blocked", Reason: o.blocked, Outputs: o.outputs}
	case o.err != nil:
		return Result{Outcome: "failed", Reason: o.err.Error(), Retryable: o.retryable, Outputs: o.outputs}
	}
	return Result{Outcome: "succeeded", Result: o.result, Outputs: o.outputs}
}

// executor runs steps for one registered worker.
type executor struct {
	cfg      Config
	workerID string
	// git is the absolute path of the git binary, resolved before any
	// session ran, so a git a session puts on PATH is never run.
	git string
	// sweep kills every other process in the sandbox; nil when the worker
	// is not contained.
	sweep func(context.Context) error
	// exhausted are the harnesses that hit their usage limit, by name.
	exhausted map[string]bool
}

// stage reports progress on the step; a failure is logged, not fatal.
func (e *executor) stage(ctx context.Context, claim Claim, stage string, outputs ...Output) {
	if err := e.cfg.Client.Stage(ctx, claim.Step.ID, e.workerID, stage, outputs); err != nil {
		e.cfg.Log.Warn("could not report the stage", "step_id", claim.Step.ID, "stage", stage, "err", err)
	}
}

// tokens is the code host token source for repo on the claim's item.
func (e *executor) tokens(claim Claim, repo string) *codehost.RefreshingToken {
	return &codehost.RefreshingToken{Fetch: func(ctx context.Context) (codehost.Token, error) {
		return e.cfg.Client.CodehostToken(ctx, claim.Item.ID, e.workerID, repo)
	}}
}

// run runs one claimed step by kind.
func (e *executor) run(ctx context.Context, claim Claim) outcome {
	s, err := newStepRun(e, claim)
	if err != nil {
		return final(err)
	}
	switch claim.Step.Kind {
	case "workspace":
		return s.workspace(ctx)
	case "implement":
		return s.implement(ctx)
	case "gate":
		return s.gate(ctx)
	case "secret_scan":
		return s.secretScan(ctx)
	case "open_pr":
		return s.openPR(ctx)
	case "review":
		return s.review(ctx)
	case "fix":
		return s.fix(ctx)
	case "merge":
		return s.merge(ctx)
	}
	return final(fmt.Errorf("this worker cannot run a %q step", claim.Step.Kind))
}
