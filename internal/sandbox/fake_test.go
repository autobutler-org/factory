package sandbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFakeLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()

	if _, err := f.Status(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Status before Ensure: err = %v, want ErrNotFound", err)
	}

	got, err := f.Ensure(ctx, "w1", "item/o/r", "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got.Name != "w1" || got.Key != "item/o/r" || got.Phase != PhasePending {
		t.Errorf("Ensure = %+v, want w1 pending", got)
	}

	f.SetPhase("w1", PhaseRunning)
	again, err := f.Ensure(ctx, "w1", "item/o/r", "")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again.Phase != PhaseRunning {
		t.Errorf("second Ensure phase = %q, want running: Ensure must not reset an existing sandbox", again.Phase)
	}

	if w2, err := f.Ensure(ctx, "w2", "item/o/q", "ghcr.io/o/x:1"); err != nil || w2.Image != "ghcr.io/o/x:1" {
		t.Fatalf("Ensure w2 = %+v, %v; want its image recorded", w2, err)
	}
	list, err := f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Key != "item/o/r" || list[0].Phase != PhaseRunning || list[0].Created.IsZero() || list[1].Name != "w2" {
		t.Errorf("List = %+v, want w1 and w2 in name order", list)
	}

	if err := f.Destroy(ctx, "w1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := f.Destroy(ctx, "w1"); err != nil {
		t.Errorf("Destroy of a missing sandbox: %v, want nil", err)
	}
	if _, err := f.Status(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Status after Destroy: err = %v, want ErrNotFound", err)
	}
}

func TestFakeEnsureRejectsEmptyName(t *testing.T) {
	t.Parallel()
	if _, err := NewFake().Ensure(t.Context(), "", "k", ""); err == nil {
		t.Error("Ensure(\"\") succeeded, want an error")
	}
}

func TestFakeExec(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()

	var out bytes.Buffer
	err := f.Exec(ctx, "missing", ExecRequest{Command: []string{"true"}, Stdout: &out})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Exec on a missing sandbox: err = %v, want ErrNotFound", err)
	}

	if _, err := f.Ensure(ctx, "w1", "item/o/r", ""); err != nil {
		t.Fatal(err)
	}
	err = f.Exec(ctx, "w1", ExecRequest{Command: []string{"git", "status"}, Stdin: strings.NewReader("in"), Stdout: &out})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Exec on a pending sandbox: err = %v, want ErrNotRunning", err)
	}

	f.SetPhase("w1", PhaseRunning)
	if err := f.Exec(ctx, "w1", ExecRequest{Command: []string{"git", "status"}, Stdin: strings.NewReader("in"), Stdout: &out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, want := out.String(), "w1: git status\nin"; got != want {
		t.Errorf("Exec output = %q, want %q", got, want)
	}

	if err := f.Exec(ctx, "w1", ExecRequest{}); err == nil {
		t.Error("Exec with no command succeeded, want an error")
	}
}

func TestFakeLogs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	if _, err := f.Logs(ctx, "w1", LogQuery{Tail: 10}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Logs of a missing sandbox: err = %v, want ErrNotFound", err)
	}
	if _, err := f.Ensure(ctx, "w1", "item/o/r", ""); err != nil {
		t.Fatal(err)
	}
	var ns *NotStartedError
	if _, err := f.Logs(ctx, "w1", LogQuery{Tail: 10}); !errors.As(err, &ns) {
		t.Errorf("Logs of a pending sandbox: err = %v, want a NotStartedError", err)
	}
	f.SetPhase("w1", PhaseRunning)
	for i, text := range []string{"a", "b", "c"} {
		f.AppendLog("w1", t0.Add(time.Duration(i)*time.Second), text)
	}
	tail, err := f.Logs(ctx, "w1", LogQuery{Tail: 2})
	if err != nil || texts(tail.Lines) != "b,c" {
		t.Fatalf("tail 2 = %q, %v; want b,c", texts(tail.Lines), err)
	}
	f.AppendLog("w1", t0.Add(3*time.Second), "d")
	next, err := f.Logs(ctx, "w1", LogQuery{After: &tail.Next})
	if err != nil || texts(next.Lines) != "d" {
		t.Errorf("after the tail = %q, %v; want d", texts(next.Lines), err)
	}
}
