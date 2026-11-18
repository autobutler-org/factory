// Package lease holds the rules of the claim lease protocol, independent of
// what is leased: the timing policy, who may act on a claimed thing, and
// whether a failure is retried or given up on.
//
// It has no SQL and no state of its own. Packages that lease rows (today
// internal/work, for work items and their steps) read the claim columns,
// ask these functions, and write the result. Expiry and stuck detection run in SQL against the
// database clock, using the durations from Policy. See
// docs/architecture/workers.md.
package lease

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotClaimant is returned when the caller does not hold the claim.
	ErrNotClaimant = errors.New("caller does not hold the claim")
	// ErrExpired is returned when the caller held the claim but its lease
	// ran out. The caller must stop working on it.
	ErrExpired = errors.New("lease expired; stop working on it")
)

// Policy is the timing of the lease protocol.
type Policy struct {
	// Lease is how long a claim lasts without a heartbeat. A worker silent
	// for this long is marked gone.
	Lease time.Duration
	// StuckAfter is how long a claim may go without a stage report, even
	// with heartbeats, before it is revoked and counted as an attempt.
	StuckAfter time.Duration
	// MaxAttempts is how many failed attempts are allowed before giving up.
	MaxAttempts int
	// RetryDelay is how long a retried thing waits before it is handed out.
	RetryDelay time.Duration
}

// DefaultPolicy is schmeide's timing: five-minute leases, a three-hour stuck
// watchdog, three attempts.
func DefaultPolicy() Policy {
	return Policy{Lease: 5 * time.Minute, StuckAfter: 3 * time.Hour, MaxAttempts: 3, RetryDelay: time.Minute}
}

// Validate rejects a policy that could not work.
func (p Policy) Validate() error {
	switch {
	case p.Lease <= 0:
		return fmt.Errorf("lease must be positive, got %s", p.Lease)
	case p.StuckAfter <= 0:
		return fmt.Errorf("stuck-after must be positive, got %s", p.StuckAfter)
	case p.MaxAttempts < 1:
		return fmt.Errorf("max-attempts must be at least 1, got %d", p.MaxAttempts)
	case p.RetryDelay < 0:
		return fmt.Errorf("retry-delay must not be negative, got %s", p.RetryDelay)
	}
	return nil
}

// Check says whether worker may act on a thing claimed by claimedBy whose
// lease is live (by the database clock).
func Check(claimedBy *string, worker string, live bool) error {
	switch {
	case worker == "" || claimedBy == nil || *claimedBy != worker:
		return ErrNotClaimant
	case !live:
		return ErrExpired
	}
	return nil
}

// Next is what happens after a failed attempt.
type Next int

const (
	// Retry hands the thing out again after RetryDelay.
	Retry Next = iota
	// GiveUp marks it failed.
	GiveUp
)

func (n Next) String() string {
	if n == Retry {
		return "retry"
	}
	return "give up"
}

// AfterFailure decides, given the attempt count including this failure,
// whether to retry.
func (p Policy) AfterFailure(attempts int, retryable bool) Next {
	if retryable && attempts < p.MaxAttempts {
		return Retry
	}
	return GiveUp
}
