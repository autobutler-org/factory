// Package sandbox starts, tracks, and destroys the sandboxes that factory
// workers run in, and opens exec sessions into them. A sandbox holds one
// worker process (`factory worker`, issue #11), which registers with the
// factory server and claims work over the worker protocol (issue #6).
//
// It defines the Runtime interface, an in-memory Fake for tests, Jobs (the
// Kubernetes Job implementation chosen in docs/decisions/0001-agent-runtime.md),
// Reconcile, which decides which chains of steps get a sandbox and which
// sandboxes may stop, and AllowImage, which says whether a sandbox may run
// the image a repository asks for. One sandbox serves one chain: the steps of one work
// item and repository that share a working tree (their sandbox key).
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
	"time"
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
	// Name is the caller-chosen name the sandbox was ensured with. The
	// worker inside registers under it.
	Name string
	// Key is the sandbox key of the chain of steps the sandbox serves
	// (`<item id>/<repo>`, docs/architecture/graph.md); its worker claims
	// only that chain's steps.
	Key string
	// Image is the image the sandbox's worker runs in.
	Image string
	// Phase is its current phase.
	Phase Phase
	// Created is when the sandbox was created.
	Created time.Time
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
	// Ensure creates the named sandbox, serving the chain whose sandbox key
	// is key and running image (empty means the runtime's default image),
	// if it does not exist, and returns its state. Ensuring an existing
	// sandbox changes nothing. The caller checks the image is allowed.
	Ensure(ctx context.Context, name, key, image string) (Sandbox, error)
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

// Plan is what Reconcile decided.
type Plan struct {
	// Start lists the sandbox keys of the chains that get a new sandbox.
	Start []string
	// Stop lists live sandboxes whose chain has nothing runnable. They are
	// only candidates: the caller stops one only after checking, under the
	// item's lock, that its chain has no unfinished step left.
	Stop []Sandbox
	// Free is how many more sandboxes the cap allows after Start.
	Free int
}

// Reconcile decides which chains get a sandbox: every key in runnable (the
// chains with a step a worker could claim now, in the order they should be
// served) that has no live sandbox, without going above maxLive live
// sandboxes. Live sandboxes whose key is not runnable are stop candidates; a
// sandbox without a key was not started by factory's launcher and is
// counted but never stopped. Ended sandboxes count for nothing.
func Reconcile(sandboxes []Sandbox, runnable []string, maxLive int) Plan {
	var p Plan
	live := 0
	covered := map[string]bool{}
	want := map[string]bool{}
	for _, k := range runnable {
		want[k] = true
	}
	for _, sb := range sandboxes {
		if !sb.Phase.Live() {
			continue
		}
		live++
		covered[sb.Key] = true
		if sb.Key != "" && !want[sb.Key] {
			p.Stop = append(p.Stop, sb)
		}
	}
	room := max(maxLive-live, 0)
	for _, k := range runnable {
		if len(p.Start) == room {
			break
		}
		if !covered[k] {
			covered[k] = true
			p.Start = append(p.Start, k)
		}
	}
	p.Free = room - len(p.Start)
	return p
}
