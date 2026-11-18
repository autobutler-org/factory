// Package sandbox starts, tracks, and destroys the sandboxes that factory
// workers run in, and opens exec sessions into them. A sandbox holds one
// worker process (`factory worker`, issue #11), which registers with the
// factory server and claims work over the worker protocol (issue #6).
//
// It defines the Runtime interface, an in-memory Fake for tests, Jobs (the
// Kubernetes Job implementation chosen in docs/decisions/0001-agent-runtime.md),
// and Reconcile, which decides how many sandboxes to start or stop.
//
// It does not claim work, pick which work item a sandbox runs, authorize
// exec sessions, or record events: the worker claims, and the factory server
// authorizes and records. It holds no state of its own; the cluster is the
// record of which sandboxes exist.
package sandbox

import (
	"context"
	"errors"
	"io"
)

// Sentinel errors returned by every Runtime.
var (
	// ErrNotFound means no sandbox has the given name.
	ErrNotFound = errors.New("sandbox not found")
	// ErrNotRunning means the sandbox exists but cannot take an exec session yet,
	// or no longer can.
	ErrNotRunning = errors.New("sandbox is not running")
)

// Phase is where a sandbox is in its life.
type Phase string

// The phases a sandbox moves through. A sandbox starts Pending, becomes Running
// once its worker process is up, and ends Succeeded or Failed.
const (
	PhasePending   Phase = "pending"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
)

// Live reports whether a sandbox in this phase still counts against the
// configured maximum.
func (p Phase) Live() bool {
	return p == PhasePending || p == PhaseRunning
}

// Sandbox is a snapshot of one sandbox's state.
type Sandbox struct {
	// Name is the caller-chosen name the sandbox was ensured with.
	Name string
	// Phase is its current phase.
	Phase Phase
}

// ExecRequest is one exec session: the command to run and the streams to
// connect to it. Stdin, Stdout, and Stderr may be nil.
type ExecRequest struct {
	Command []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	TTY     bool
}

// Runtime is what factory needs from the system that runs worker sandboxes.
type Runtime interface {
	// Ensure creates the named sandbox if it does not exist and returns its
	// state. Ensuring an existing sandbox changes nothing.
	Ensure(ctx context.Context, name string) (Sandbox, error)
	// Status returns the named sandbox's state, or ErrNotFound.
	Status(ctx context.Context, name string) (Sandbox, error)
	// List returns every sandbox this runtime manages, in name order.
	List(ctx context.Context) ([]Sandbox, error)
	// Destroy stops and removes the named sandbox. The worker inside gets
	// SIGTERM first. Destroying a missing sandbox is not an error.
	Destroy(ctx context.Context, name string) error
	// Exec runs a command in the named sandbox's worker container, streaming
	// its input and output until the command exits or ctx is canceled.
	Exec(ctx context.Context, name string, req ExecRequest) error
}

// Counts is what Reconcile decides from.
type Counts struct {
	// Ready is the number of work items in the ready state.
	Ready int
	// Busy is the number of live sandboxes whose worker holds a claim.
	Busy int
	// Idle is the number of live sandboxes whose worker holds no claim,
	// including ones still starting that have not registered yet.
	Idle int
	// Max is the most live sandboxes allowed at once.
	Max int
}

// Plan is what Reconcile decided: how many sandboxes to start, and how many
// idle ones to stop. Busy sandboxes are never stopped.
type Plan struct {
	Start int
	Stop  int
}

// Reconcile decides how many sandboxes to start or stop so that every ready
// item has an idle sandbox to claim it, no idle sandbox sits without work, and
// no more than Max sandboxes are live. A busy sandbox is never stopped: its
// worker exits on its own when its item is done. Negative counts are treated
// as zero.
func Reconcile(c Counts) Plan {
	ready, busy, idle, maxLive := max(c.Ready, 0), max(c.Busy, 0), max(c.Idle, 0), max(c.Max, 0)
	room := max(maxLive-busy-idle, 0)
	start := min(max(ready-idle, 0), room)
	// Idle sandboxes beyond the ready work, or beyond what the cap leaves
	// after the busy ones, have nothing to do.
	keep := min(ready, max(maxLive-busy, 0))
	stop := max(idle-keep, 0)
	return Plan{Start: start, Stop: stop}
}
