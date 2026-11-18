package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/harness"
)

// Ported from schmeide's internal/runner/runner.go (runHarness and
// continueOnLimit): one agent session with a whole-session deadline, a kill
// when its output goes silent, and a move to the next harness when one hits
// its usage limit.

// DefaultIdleLimit is how long a session may write no output before it is
// killed.
const DefaultIdleLimit = 15 * time.Minute

var (
	// errSessionIdle is the cancel cause when a session's output goes silent.
	errSessionIdle = errors.New("the session produced no output")
	// ErrOutOfUsage is a session no harness is left for: every one hit its
	// usage limit in this process.
	ErrOutOfUsage = errors.New("every harness is at its usage limit")
)

// idleWriter passes writes to w and pushes the idle deadline back on each one.
type idleWriter struct {
	w     io.Writer
	timer *time.Timer
	limit time.Duration
}

func (i idleWriter) Write(p []byte) (int, error) {
	i.timer.Reset(i.limit)
	if i.w == nil {
		return len(p), nil
	}
	return i.w.Write(p)
}

// session runs one agent session in dir. When a harness ends at its usage
// limit, the same request runs again on the next harness, with a note on how
// far the last one got. Harnesses that hit their limit are skipped for the
// rest of the process.
func (e *executor) session(ctx context.Context, dir, prompt string, limit time.Duration) error {
	req := harness.Request{Dir: dir, Prompt: prompt, Env: harnessEnv(os.Environ()), Output: e.cfg.Output}
	for _, h := range e.cfg.Harnesses {
		if e.exhausted[h.Name()] {
			continue
		}
		err := e.runHarness(ctx, h, limit, req)
		if !errors.Is(err, harness.ErrUsageLimit) {
			return err
		}
		e.exhausted[h.Name()] = true
		var limited *harness.UsageLimitError
		tail := ""
		if errors.As(err, &limited) {
			tail = limited.Tail
		}
		e.cfg.Log.Warn("harness ran out of usage; continuing on the next one", "harness", h.Name(), "dir", dir)
		req.Prompt = continuation(h.Name(), tail) + prompt
	}
	return fmt.Errorf("%w; the step is retried later", ErrOutOfUsage)
}

// runHarness runs one session of h. limit bounds the whole session and the
// idle limit the silence between writes; either kills the session.
func (e *executor) runHarness(ctx context.Context, h harness.Harness, limit time.Duration, req harness.Request) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	idle := e.cfg.IdleLimit
	timer := time.AfterFunc(idle, func() { stop(errSessionIdle) })
	defer timer.Stop()
	req.Output = idleWriter{w: req.Output, timer: timer, limit: idle}
	err := h.Run(ctx, req)
	switch {
	case err == nil:
		return nil
	case errors.Is(context.Cause(ctx), errSessionIdle):
		return fmt.Errorf("the %s session produced no output for %s and was killed", h.Name(), idle)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("the %s session timed out after %s", h.Name(), limit)
	}
	return fmt.Errorf("the %s session did not finish: %w", h.Name(), err)
}

// continuation is the note for the harness that takes over from name.
func continuation(name, tail string) string {
	return fmt.Sprintf(`You are continuing a session that ran out of usage. The %s harness started the task below and stopped partway through when its account hit a usage limit. Its partial work is already in this directory, uncommitted; run git status and git diff to see it. Continue the task from where it stopped. Do not start over, and keep its work unless it is wrong.

The end of the %s session's output:
%s

The task:

`, name, name, strings.TrimSpace(tail))
}
