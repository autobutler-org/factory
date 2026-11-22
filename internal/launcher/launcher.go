// Package launcher keeps the cluster's worker sandboxes in step with the
// step graphs, from inside the factory server, and runs the steps that need
// no sandbox. Each pass of Launcher.Pass:
//
//  0. polls the code host (when a GitHub App is set), at most every
//     PollInterval: one PullStatus read per open pull request of an item
//     being worked, completing a ready await_checks step when the head
//     commit's checks pass or fail and a ready await_approval step when the
//     pull request is approved, as the actor system/poller;
//  1. runs the server-side steps (merge) through the step protocol, as the
//     server's own worker, with the code host (when a GitHub App is set);
//  2. plans ready items while there is room under the sandbox cap, since a
//     sandbox's worker claims only its own chain and never plans an item;
//  3. starts one sandbox for every chain (sandbox key) with a runnable step
//     and no live sandbox, up to the cap (sandbox.Reconcile), in the image
//     the chain's repository names when AllowedImages allows it, or the
//     runtime's default; a chain whose image is not allowed blocks its item;
//  4. destroys a live sandbox only when its chain has no step left to run,
//     under the item's lock (work.Service.EndIdleSandbox), and one that has
//     stayed pending past PendingTimeout, whose worker never started.
//
// A pass holds a Postgres advisory lock, so with several server replicas
// one pass runs at a time. It does not claim sandbox steps, pick which step a
// sandbox runs (the worker claims), or delete ended sandboxes: Kubernetes
// removes a finished Job an hour after it ends, so its logs stay readable.
package launcher

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the advisory lock every replica's launcher takes for a pass.
const lockKey int64 = 0x666163746f7279 // "factory"

// Launcher is the server's sandbox launcher and server-side step runner.
type Launcher struct {
	// Work is the work item service.
	Work *work.Service
	// Pool is the database the advisory lock is taken in.
	Pool *pgxpool.Pool
	// Runtime starts and destroys sandboxes.
	Runtime sandbox.Runtime
	// MaxSandboxes caps the live sandboxes.
	MaxSandboxes int
	// Capabilities are what the launched workers can run: an item whose
	// requirements are not all here is neither planned nor given a sandbox.
	Capabilities []string
	// Host returns the code host for a repository, for server-side steps.
	// Nil means no GitHub App is configured: server-side steps stay ready.
	Host func(ctx context.Context, repo string) (codehost.Host, error)
	// AllowedImages are the image repository prefixes a chain's sandbox may
	// run instead of the runtime's default (sandbox.AllowImage). The image is
	// checked when the item is planned and again here, before it runs.
	AllowedImages []string
	// PendingTimeout is how long a sandbox may stay pending (an image that
	// will not pull, a pod that cannot be scheduled) before it is destroyed
	// and its chain gets a new one. Zero never times one out. A pending
	// sandbox's worker has not started, so it holds no step.
	PendingTimeout time.Duration
	// PollInterval is how often a pass also polls the code host for the
	// open pull requests of the items being worked (Host must be set): a
	// pass polls when this long has gone by since the last poll. Zero polls
	// on every pass.
	PollInterval time.Duration
	// ChecksGrace is how long an await_checks step waits on a head commit
	// with no checks at all, from when the step became ready or the commit
	// was made, whichever is later, before it passes: a repository with no
	// CI must not wait forever. Zero passes at once.
	ChecksGrace time.Duration
	// Logger receives one line per pass that changed something.
	Logger *slog.Logger

	// lastPoll is when this process last polled. Each replica keeps its own.
	lastPoll time.Time
}

// Report counts what one pass did.
type Report struct {
	// Locked is false when another replica held the lock and the pass did
	// nothing.
	Locked bool
	// Polled counts the changes the code host poll made.
	Polled      int
	ServerSteps int
	Planned     int
	Started     []string
	Stopped     []string
}

func (r Report) changed() bool {
	return r.Polled+r.ServerSteps+r.Planned+len(r.Started)+len(r.Stopped) > 0
}

// Pass runs one pass, unless another replica is running one.
func (l *Launcher) Pass(ctx context.Context) (Report, error) {
	var r Report
	locked, err := db.TryLock(ctx, l.Pool, lockKey, func(ctx context.Context) error {
		var err error
		r, err = l.pass(ctx)
		return err
	})
	r.Locked = locked
	return r, err
}

func (l *Launcher) pass(ctx context.Context) (Report, error) {
	var r Report
	// ponytail: each replica polls on its own interval, so two replicas can
	// double the requests; keep the last poll in Postgres if that matters.
	if l.Host != nil && time.Since(l.lastPoll) >= l.PollInterval {
		l.lastPoll = time.Now()
		n, err := l.poll(ctx)
		r.Polled = n
		if err != nil {
			return r, err
		}
	}
	if l.Host != nil {
		n, err := l.runServerSteps(ctx)
		r.ServerSteps = n
		if err != nil {
			return r, err
		}
	}
	sandboxes, err := l.Runtime.List(ctx)
	if err != nil {
		return r, err
	}
	sandboxes, err = l.dropStuckPending(ctx, sandboxes, &r)
	if err != nil {
		return r, err
	}
	runnable, err := l.Work.RunnableSandboxes(ctx, l.Capabilities)
	if err != nil {
		return r, err
	}
	p := sandbox.Reconcile(sandboxes, runnable, l.MaxSandboxes)
	if p.Free > 0 {
		if r.Planned, err = l.Work.StartReady(ctx, l.Capabilities, p.Free); err != nil {
			return r, err
		}
		if r.Planned > 0 {
			if runnable, err = l.Work.RunnableSandboxes(ctx, l.Capabilities); err != nil {
				return r, err
			}
			p = sandbox.Reconcile(sandboxes, runnable, l.MaxSandboxes)
		}
	}
	for _, key := range p.Start {
		image, err := l.Work.SandboxImage(ctx, key)
		if err != nil {
			return r, err
		}
		if image != "" {
			if err := sandbox.AllowImage(image, l.AllowedImages); err != nil {
				if err := l.block(ctx, key, err); err != nil {
					return r, err
				}
				continue
			}
		}
		sb, err := l.Runtime.Ensure(ctx, sandboxName(), key, image)
		if err != nil {
			return r, fmt.Errorf("start a sandbox for %s: %w", key, err)
		}
		r.Started = append(r.Started, sb.Name)
	}
	for _, sb := range p.Stop {
		ended, err := l.Work.EndIdleSandbox(ctx, sb.Key, func(ctx context.Context) error { return l.Runtime.Destroy(ctx, sb.Name) })
		if err != nil {
			return r, fmt.Errorf("stop sandbox %s: %w", sb.Name, err)
		}
		if ended {
			r.Stopped = append(r.Stopped, sb.Name)
		}
	}
	return r, nil
}

// block moves the item of the chain with the given sandbox key to blocked, because
// the image its repository names may not run here.
func (l *Launcher) block(ctx context.Context, key string, why error) error {
	itemID, repo, _ := strings.Cut(key, "/")
	reason := fmt.Sprintf("%s: %v", repo, why)
	if _, err := l.Work.Transition(ctx, itemID, work.Blocked, reason, work.Actor{Kind: work.ActorSystem, ID: "launcher"}); err != nil {
		return fmt.Errorf("block %s: %w", itemID, err)
	}
	l.Logger.Warn("blocked an item: its repository's image is not allowed", "sandbox_key", key, "reason", reason)
	return nil
}

// dropStuckPending destroys the sandboxes pending for longer than
// PendingTimeout and returns the others.
func (l *Launcher) dropStuckPending(ctx context.Context, sandboxes []sandbox.Sandbox, r *Report) ([]sandbox.Sandbox, error) {
	if l.PendingTimeout <= 0 {
		return sandboxes, nil
	}
	kept := make([]sandbox.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.Phase != sandbox.PhasePending || sb.Created.IsZero() || time.Since(sb.Created) < l.PendingTimeout {
			kept = append(kept, sb)
			continue
		}
		if err := l.Runtime.Destroy(ctx, sb.Name); err != nil {
			return nil, fmt.Errorf("destroy sandbox %s, pending too long: %w", sb.Name, err)
		}
		l.Logger.Warn("destroyed a sandbox pending too long; check its pod events for an image pull or scheduling error",
			"sandbox", sb.Name, "sandbox_key", sb.Key, "pending_for", time.Since(sb.Created).Round(time.Second).String(), "timeout", l.PendingTimeout.String())
		r.Stopped = append(r.Stopped, sb.Name)
	}
	return kept, nil
}

// sandboxName is a new Job name: unique, so a chain whose last sandbox has
// ended but not yet been deleted can get another, and a valid DNS label.
func sandboxName() string {
	return "worker-" + strings.ToLower(rand.Text())[:12]
}

// Run runs a pass every interval until ctx ends, logging what changed and
// any error. Errors do not stop the loop; the next pass tries again.
func (l *Launcher) Run(ctx context.Context, interval time.Duration) {
	if l.Host == nil {
		l.Logger.Warn("server-side steps (merge) will not run and the code host is not polled: no GitHub App is configured. " +
			"Set --github-app-id and --github-app-key-file; until then merge and wait steps stay ready")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r, err := l.Pass(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			l.Logger.Error("launcher pass failed", "err", err)
		case r.changed():
			l.Logger.Info("launcher pass", "polled", r.Polled, "server_steps", r.ServerSteps, "planned", r.Planned, "started", r.Started, "stopped", r.Stopped)
		}
	}
}
