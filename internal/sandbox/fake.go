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
	"time"
)

// Fake is an in-memory Runtime for tests. Ensure creates a pending sandbox;
// tests move it with SetPhase. Exec on a running sandbox writes
// "<name>: <command>\n" to Stdout, then copies Stdin to Stdout.
type Fake struct {
	mu        sync.Mutex
	sandboxes map[string]Sandbox
}

var _ Runtime = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{sandboxes: map[string]Sandbox{}}
}

// SetPhase moves the named sandbox to phase, creating it if needed.
func (f *Fake) SetPhase(name string, phase Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.sandboxes[name]
	sb.Name, sb.Phase = name, phase
	f.sandboxes[name] = sb
}

// SetCreated sets when the named sandbox was created, so a test can age it.
func (f *Fake) SetCreated(name string, created time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.sandboxes[name]
	sb.Name, sb.Created = name, created
	f.sandboxes[name] = sb
}

// Ensure implements Runtime.
func (f *Fake) Ensure(_ context.Context, name, key string) (Sandbox, error) {
	if name == "" {
		return Sandbox{}, errors.New("ensure sandbox: name is empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		f.sandboxes[name] = Sandbox{Name: name, Key: key, Phase: PhasePending, Created: time.Now()}
	}
	return f.sandboxes[name], nil
}

// Status implements Runtime.
func (f *Fake) Status(_ context.Context, name string) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return Sandbox{}, fmt.Errorf("sandbox %q: %w", name, ErrNotFound)
	}
	return sb, nil
}

// List implements Runtime.
func (f *Fake) List(_ context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := slices.Sorted(maps.Keys(f.sandboxes))
	out := make([]Sandbox, 0, len(names))
	for _, name := range names {
		out = append(out, f.sandboxes[name])
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
