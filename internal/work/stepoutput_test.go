package work_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/work"
)

func TestStepOutput(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	a, b := register(t, svc, "a"), register(t, svc, "b")
	item := mustCreate(t, svc, work.NewItem{Title: "t", Repos: []string{"o/a"}})
	c := mustClaim(t, svc, a.ID, "o/a/workspace")

	if _, _, err := svc.StepOutput(ctx, c.Step.ID); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("output before an upload: %v, want ErrNotFound", err)
	}
	if _, err := svc.PutStepOutput(ctx, c.Step.ID, b.ID, []byte("x")); !errors.Is(err, lease.ErrNotClaimant) {
		t.Errorf("upload by another worker: %v, want ErrNotClaimant", err)
	}
	if _, err := svc.PutStepOutput(ctx, c.Step.ID, a.ID, make([]byte, work.MaxStepOutputBytes+1)); !errors.Is(err, work.ErrInvalid) {
		t.Errorf("upload past the cap: %v, want ErrInvalid", err)
	}
	full := bytes.Repeat([]byte("y"), work.MaxStepOutputBytes)
	info, err := svc.PutStepOutput(ctx, c.Step.ID, a.ID, full)
	if err != nil || info.Bytes != int64(len(full)) || info.StepID != c.Step.ID {
		t.Fatalf("upload at the cap: %+v, %v", info, err)
	}
	// Bytes that are not text are kept as they are.
	raw := []byte("line\x00\xff\n")
	if _, err := svc.PutStepOutput(ctx, c.Step.ID, a.ID, raw); err != nil {
		t.Fatal(err)
	}
	if got, _, err := svc.StepOutput(ctx, c.Step.ID); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("output = %q, %v; want the second upload", got, err)
	}
	g, err := svc.Graph(ctx, item.ID)
	if err != nil || len(g.StepOutputs) != 1 || g.StepOutputs[0].Bytes != int64(len(raw)) {
		t.Errorf("graph step outputs = %+v, %v", g.StepOutputs, err)
	}

	// An output past the retention is deleted when another is stored.
	if _, err := pool.Exec(ctx, `UPDATE step_outputs SET created_at = now() - interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportStepResult(ctx, c.Step.ID, a.ID, work.StepResult{Outcome: work.StepSucceeded}); err != nil {
		t.Fatal(err)
	}
	next := mustClaim(t, svc, a.ID, "o/a/implement")
	if _, err := svc.PutStepOutput(ctx, next.Step.ID, a.ID, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StepOutput(ctx, c.Step.ID); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("an output past the retention: %v, want ErrNotFound", err)
	}
	if _, _, err := svc.StepOutput(ctx, "not-a-uuid"); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("a malformed step ID: %v, want ErrNotFound", err)
	}
}
