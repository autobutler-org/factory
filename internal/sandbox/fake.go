package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Fake is an in-memory Runtime for tests. Ensure creates a pending sandbox;
// tests move it with SetPhase. Exec on a running sandbox writes
// "<name>: <command>\n" to Stdout, then copies Stdin to Stdout.
type Fake struct {
	mu        sync.Mutex
	sandboxes map[string]Phase
}

var _ Runtime = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{sandboxes: map[string]Phase{}}
}

// SetPhase moves the named sandbox to phase, creating it if needed.
func (f *Fake) SetPhase(name string, phase Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sandboxes[name] = phase
}

// Ensure implements Runtime.
func (f *Fake) Ensure(_ context.Context, name string) (Sandbox, error) {
	if name == "" {
		return Sandbox{}, errors.New("ensure sandbox: name is empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		f.sandboxes[name] = PhasePending
	}
	return Sandbox{Name: name, Phase: f.sandboxes[name]}, nil
}

// Status implements Runtime.
func (f *Fake) Status(_ context.Context, name string) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	phase, ok := f.sandboxes[name]
	if !ok {
		return Sandbox{}, fmt.Errorf("sandbox %q: %w", name, ErrNotFound)
	}
	return Sandbox{Name: name, Phase: phase}, nil
}

// List implements Runtime.
func (f *Fake) List(_ context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := slices.Sorted(maps.Keys(f.sandboxes))
	out := make([]Sandbox, 0, len(names))
	for _, name := range names {
		out = append(out, Sandbox{Name: name, Phase: f.sandboxes[name]})
	}
	return out, nil
}

// Destroy implements Runtime.
func (f *Fake) Destroy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, name)
	return nil
}

// Exec implements Runtime.
func (f *Fake) Exec(ctx context.Context, name string, req ExecRequest) error {
	if len(req.Command) == 0 {
		return errors.New("exec: command is empty")
	}
	sb, err := f.Status(ctx, name)
	if err != nil {
		return err
	}
	if sb.Phase != PhaseRunning {
		return fmt.Errorf("sandbox %q is %s: %w", name, sb.Phase, ErrNotRunning)
	}
	if req.Stdout == nil {
		return nil
	}
	if _, err := fmt.Fprintf(req.Stdout, "%s: %s\n", name, strings.Join(req.Command, " ")); err != nil {
		return fmt.Errorf("exec: write output: %w", err)
	}
	if req.Stdin != nil {
		if _, err := io.Copy(req.Stdout, req.Stdin); err != nil {
			return fmt.Errorf("exec: copy input: %w", err)
		}
	}
	return nil
}
