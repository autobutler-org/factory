package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/apiclient"
)

// ErrNetworkNotReady is a call that kept failing to resolve or reach its
// host for the whole of the worker's network wait. A fresh sandbox pod can
// have no working DNS for its first seconds (#76).
var ErrNetworkNotReady = errors.New("the network was not ready")

// netWait is how long, and how often, the worker retries a call that failed
// before it reached its host: a resolver error, a refused or timed out
// connection.
type netWait struct {
	// first is the delay after the first failure; each later one doubles,
	// up to most.
	first, most time.Duration
	// total bounds the whole wait.
	total time.Duration
}

// The default wait gives a slow start two minutes.
const (
	netWaitFirst = 500 * time.Millisecond
	netWaitMost  = 10 * time.Second
	netWaitTotal = 2 * time.Minute
)

// orDefault is w, or the default wait when w is zero.
func (w netWait) orDefault() netWait {
	if w.total <= 0 {
		return netWait{first: netWaitFirst, most: netWaitMost, total: netWaitTotal}
	}
	return w
}

// do calls fn until it succeeds, fails with an error transient does not
// match, or the wait runs out, which wraps the last error in
// ErrNetworkNotReady. what names the call in logs and the error.
func (w netWait) do(ctx context.Context, log *slog.Logger, what string, transient func(error) bool, fn func() error) error {
	w = w.orDefault()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	start := time.Now()
	delay := w.first
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !transient(err) {
			return err
		}
		waited := time.Since(start)
		if waited+delay > w.total {
			return fmt.Errorf("%w: %s still failed after %d attempts in %s: %w. "+
				"Check that the worker's pod can resolve and reach the host: cluster DNS and the worker egress policy (docs/operations.md)",
				ErrNetworkNotReady, what, attempt, waited.Round(time.Millisecond), err)
		}
		log.Warn("the network is not ready; waiting", "call", what, "attempt", attempt, "delay", delay, "err", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		delay = min(delay*2, w.most)
	}
}

// unreachable says the factory server gave no reply at all.
func unreachable(err error) bool { return errors.Is(err, apiclient.ErrUnreachable) }

// gitUnreachable says a git command failed before it reached the remote:
// its output carries what git, through curl, prints for a resolver or
// connection failure.
func gitUnreachable(err error) bool {
	msg := err.Error()
	for _, s := range [...]string{
		"Could not resolve host",
		"Could not resolve proxy",
		"Temporary failure in name resolution",
		"Failed to connect to",
		"Couldn't connect to server",
		"Could not connect to server",
		"Connection refused",
		"Connection timed out",
		"Network is unreachable",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
