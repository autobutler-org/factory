package work

import (
	"context"
	"fmt"
	"time"

	"github.com/autobutler-org/factory/internal/db"
)

const (
	// MaxStepOutputBytes caps the output kept for one step: the worker cuts
	// a larger gate output to its head and tail before it uploads it, and
	// the step_outputs table refuses more.
	MaxStepOutputBytes = 5 << 20
	// StepOutputRetention is how long a step's output is kept. Older ones
	// are deleted whenever a new one is stored.
	StepOutputRetention = 30 * 24 * time.Hour
)

// StepOutputInfo is what is kept of a step's output, without the output.
type StepOutputInfo struct {
	StepID    string    `json:"step_id"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// PutStepOutput keeps the combined output of a gate run of the step, which
// workerID must hold a live lease on. It replaces an earlier run's output and
// deletes every output older than StepOutputRetention. An output larger
// than MaxStepOutputBytes is ErrInvalid.
func (s *Service) PutStepOutput(ctx context.Context, stepID, workerID string, output []byte) (StepOutputInfo, error) {
	if len(output) > MaxStepOutputBytes {
		return StepOutputInfo{}, fmt.Errorf("%w: the output is %d bytes; cut it to at most %d", ErrInvalid, len(output), MaxStepOutputBytes)
	}
	var info StepOutputInfo
	_, err := s.asStepClaimant(ctx, stepID, workerID, func(q *db.Queries, _ db.WorkItem, step db.WorkStep, _ Actor) (db.WorkStep, error) {
		if _, err := q.PruneStepOutputs(ctx, time.Now().Add(-StepOutputRetention)); err != nil {
			return db.WorkStep{}, fmt.Errorf("delete old step outputs: %w", err)
		}
		if output == nil {
			output = []byte{}
		}
		row, err := q.PutStepOutput(ctx, db.PutStepOutputParams{StepID: stepID, Output: output})
		if err != nil {
			return db.WorkStep{}, fmt.Errorf("store the step's output: %w", err)
		}
		info = StepOutputInfo(row)
		return step, nil
	})
	return info, err
}

// StepOutput returns the output kept for the step and when it was stored.
// A step without one, or no such step, is ErrNotFound.
func (s *Service) StepOutput(ctx context.Context, stepID string) ([]byte, time.Time, error) {
	row, err := db.New(s.pool).GetStepOutput(ctx, stepID)
	if err != nil {
		return nil, time.Time{}, notFound(err, "output of step "+stepID)
	}
	return row.Output, row.CreatedAt, nil
}
