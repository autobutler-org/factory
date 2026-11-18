package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/jackc/pgx/v5"
)

// Worker kinds and states.
const (
	WorkerCluster  = "cluster"
	WorkerLocal    = "local"
	WorkerActive   = "active"
	WorkerDraining = "draining"
	WorkerGone     = "gone"
)

// Event types written by the lease protocol.
const (
	EventClaimed      = "claimed"
	EventReleased     = "released"
	EventStage        = "stage_reported"
	EventResult       = "result_reported"
	EventLeaseExpired = "lease_expired"
	EventStuck        = "stuck"
)

// reapBatch caps how many rows one reaper pass takes, so a backlog cannot
// hold locks for long.
const reapBatch = 100

// ErrWorkerNotActive is returned when a worker that is draining or gone asks
// for work. A gone worker becomes active again by heartbeating or registering.
var ErrWorkerNotActive = errors.New("worker is not active: heartbeat or register again before claiming")

// Capabilities are what a worker can run. An item's Requires must all be
// among Harnesses and Labels for the worker to be handed it.
type Capabilities struct {
	Harnesses []string `json:"harnesses"`
	Labels    []string `json:"labels"`
}

func (c Capabilities) all() []string {
	return append(slices.Clone(c.Harnesses), c.Labels...)
}

// WorkerInput registers a worker.
type WorkerInput struct {
	Name         string
	Kind         string
	Capabilities Capabilities
	Version      string
}

// RegisterWorker adds a worker, or refreshes the one with the same name and
// makes it active again.
func (s *Service) RegisterWorker(ctx context.Context, in WorkerInput) (db.Worker, error) {
	switch {
	case in.Name == "":
		return db.Worker{}, fmt.Errorf("%w: worker name is required", ErrInvalid)
	case in.Kind != WorkerCluster && in.Kind != WorkerLocal:
		return db.Worker{}, fmt.Errorf("%w: worker kind must be cluster or local", ErrInvalid)
	case slices.Contains(in.Capabilities.all(), ""):
		return db.Worker{}, fmt.Errorf("%w: capabilities must not contain an empty string", ErrInvalid)
	}
	caps := in.Capabilities
	if caps.Harnesses == nil {
		caps.Harnesses = []string{}
	}
	if caps.Labels == nil {
		caps.Labels = []string{}
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		return db.Worker{}, fmt.Errorf("encode capabilities: %w", err)
	}
	w, err := db.New(s.pool).RegisterWorker(ctx, db.RegisterWorkerParams{Name: in.Name, Kind: in.Kind, Capabilities: raw, Version: in.Version})
	if err != nil {
		return db.Worker{}, fmt.Errorf("register worker: %w", err)
	}
	return w, nil
}

// Workers lists the registry by name.
func (s *Service) Workers(ctx context.Context) ([]db.Worker, error) {
	workers, err := db.New(s.pool).ListWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	return workers, nil
}

// Policy is the lease policy this service runs with.
func (s *Service) Policy() lease.Policy { return s.policy }

// Heartbeat records that a worker is alive and extends the lease on every
// item it still holds. It returns those items' IDs; anything the worker
// thinks it holds but is not listed has been lost and must be abandoned.
func (s *Service) Heartbeat(ctx context.Context, workerID string) ([]string, error) {
	var held []string
	err := s.inTx(ctx, func(q *db.Queries) error {
		if _, err := q.TouchWorker(ctx, workerID); err != nil {
			return notFound(err, "worker "+workerID)
		}
		var err error
		held, err = q.ExtendLeases(ctx, db.ExtendLeasesParams{WorkerID: &workerID, LeaseSeconds: s.policy.Lease.Seconds()})
		if err != nil {
			return fmt.Errorf("extend leases: %w", err)
		}
		return nil
	})
	return held, err
}

// ClaimNext hands the worker the highest-priority ready item it is capable
// of that has no unfinished dependency and is not waiting out a retry delay.
// ok is false when there is nothing to hand out.
func (s *Service) ClaimNext(ctx context.Context, workerID string) (item db.WorkItem, ok bool, err error) {
	err = s.inTx(ctx, func(q *db.Queries) error {
		w, err := activeWorker(ctx, q, workerID)
		if err != nil {
			return err
		}
		var caps Capabilities
		if err := json.Unmarshal(w.Capabilities, &caps); err != nil {
			return fmt.Errorf("decode capabilities of worker %s: %w", workerID, err)
		}
		id, err := q.PickNext(ctx, caps.all())
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("pick next item: %w", err)
		}
		item, err = s.claim(ctx, q, id, workerID)
		ok = err == nil
		return err
	})
	return item, ok, err
}

// Claim gives one specific ready item to a worker, whatever its capabilities
// or retry delay. Developers use it to take work to their own machine.
func (s *Service) Claim(ctx context.Context, id, workerID string) (db.WorkItem, error) {
	var item db.WorkItem
	err := s.inTx(ctx, func(q *db.Queries) error {
		if _, err := activeWorker(ctx, q, workerID); err != nil {
			return err
		}
		var err error
		item, err = s.claim(ctx, q, id, workerID)
		return err
	})
	return item, err
}

func activeWorker(ctx context.Context, q *db.Queries, workerID string) (db.Worker, error) {
	w, err := q.GetWorker(ctx, workerID)
	if err != nil {
		return db.Worker{}, notFound(err, "worker "+workerID)
	}
	if w.State != WorkerActive {
		return db.Worker{}, fmt.Errorf("worker %s is %s: %w", workerID, w.State, ErrWorkerNotActive)
	}
	if w, err = q.TouchWorker(ctx, workerID); err != nil {
		return db.Worker{}, fmt.Errorf("touch worker: %w", err)
	}
	return w, nil
}

func (s *Service) claim(ctx context.Context, q *db.Queries, id, workerID string) (db.WorkItem, error) {
	item, err := q.GetItemForUpdate(ctx, id)
	if err != nil {
		return db.WorkItem{}, notFound(err, "work item "+id)
	}
	actor := Actor{Kind: ActorWorker, ID: workerID}
	if _, err := transition(ctx, q, item, InProgress, "", actor, EventClaimed, map[string]any{"worker_id": workerID}); err != nil {
		return db.WorkItem{}, err
	}
	item, err = q.SetClaim(ctx, db.SetClaimParams{ID: id, WorkerID: &workerID, LeaseSeconds: s.policy.Lease.Seconds()})
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("set claim: %w", err)
	}
	return item, nil
}

// asClaimant locks the item and runs f only if workerID holds a live lease
// on it. Otherwise it returns lease.ErrNotClaimant or lease.ErrExpired and
// changes nothing.
func (s *Service) asClaimant(ctx context.Context, id, workerID string, f func(*db.Queries, db.WorkItem, Actor) (db.WorkItem, error)) (db.WorkItem, error) {
	var out db.WorkItem
	err := s.inTx(ctx, func(q *db.Queries) error {
		item, err := q.GetItemForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "work item "+id)
		}
		live, err := q.LeaseLive(ctx, id)
		if err != nil {
			return fmt.Errorf("read lease: %w", err)
		}
		if err := lease.Check(item.ClaimedBy, workerID, live); err != nil {
			return fmt.Errorf("work item %s, worker %s: %w", id, workerID, err)
		}
		out, err = f(q, item, Actor{Kind: ActorWorker, ID: workerID})
		return err
	})
	return out, err
}

// Release gives a claimed item back: in_progress to ready, not counted as
// an attempt.
func (s *Service) Release(ctx context.Context, id, workerID, reason string) (db.WorkItem, error) {
	return s.asClaimant(ctx, id, workerID, func(q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
		return transition(ctx, q, item, Ready, reason, actor, EventReleased, nil)
	})
}

// ReportStage records progress detail without changing state, resets the
// stuck timer, and records any outputs pushed so far.
func (s *Service) ReportStage(ctx context.Context, id, workerID, stage string, outputs []OutputInput) (db.WorkItem, error) {
	if err := validateOutputs(outputs); err != nil {
		return db.WorkItem{}, err
	}
	return s.asClaimant(ctx, id, workerID, func(q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
		for _, o := range outputs {
			if _, err := recordOutput(ctx, q, item, o, actor); err != nil {
				return db.WorkItem{}, err
			}
		}
		updated, err := q.SetStage(ctx, db.SetStageParams{ID: id, Stage: stage})
		if err != nil {
			return db.WorkItem{}, fmt.Errorf("set stage: %w", err)
		}
		_, err = writeEvent(ctx, q, id, actor, EventStage, "", "", map[string]any{"stage": stage})
		return updated, err
	})
}

// RecordWorkerOutput records one output for the worker holding the item.
func (s *Service) RecordWorkerOutput(ctx context.Context, id, workerID string, o OutputInput) (db.WorkOutput, error) {
	if err := o.validate(); err != nil {
		return db.WorkOutput{}, err
	}
	var out db.WorkOutput
	_, err := s.asClaimant(ctx, id, workerID, func(q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
		var err error
		out, err = recordOutput(ctx, q, item, o, actor)
		return item, err
	})
	return out, err
}

// Result is a worker's outcome for its item. Outcome is in_review or
// completed (the worker has nothing more to produce; its outputs decide
// which state the item lands in), blocked, or failed. Retryable applies to
// failed only.
type Result struct {
	Outcome   string
	Reason    string
	Retryable bool
	Outputs   []OutputInput
}

// ReportResult records outputs, then applies the outcome. The item leaves
// in_progress, so the claim ends.
func (s *Service) ReportResult(ctx context.Context, id, workerID string, r Result) (db.WorkItem, error) {
	if r.Outcome != InReview && r.Outcome != Completed && r.Outcome != Blocked && r.Outcome != Failed {
		return db.WorkItem{}, fmt.Errorf("%w: outcome must be in_review, completed, blocked or failed", ErrInvalid)
	}
	if err := validateOutputs(r.Outputs); err != nil {
		return db.WorkItem{}, err
	}
	return s.asClaimant(ctx, id, workerID, func(q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
		for _, o := range r.Outputs {
			if _, err := recordOutput(ctx, q, item, o, actor); err != nil {
				return db.WorkItem{}, err
			}
		}
		if _, err := writeEvent(ctx, q, id, actor, EventResult, "", "", map[string]any{
			"outcome": r.Outcome, "reason": r.Reason, "retryable": r.Retryable, "outputs": len(r.Outputs),
		}); err != nil {
			return db.WorkItem{}, err
		}
		switch r.Outcome {
		case Blocked:
			return transition(ctx, q, item, Blocked, r.Reason, actor, EventTransition, nil)
		case Failed:
			return s.failAttempt(ctx, q, item, r.Reason, r.Retryable, actor, EventTransition)
		}
		return finishOutputs(ctx, q, item, actor)
	})
}

// failAttempt counts an attempt, then retries after the policy's delay or
// gives up, as lease.Policy.AfterFailure decides.
func (s *Service) failAttempt(ctx context.Context, q *db.Queries, item db.WorkItem, reason string, retryable bool, actor Actor, eventType string) (db.WorkItem, error) {
	item, err := q.IncrementAttempts(ctx, item.ID)
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("count attempt: %w", err)
	}
	payload := map[string]any{"attempts": item.Attempts, "max_attempts": s.policy.MaxAttempts}
	if s.policy.AfterFailure(int(item.Attempts), retryable) == lease.GiveUp {
		return transition(ctx, q, item, Failed, reason, actor, eventType, payload)
	}
	if _, err := transition(ctx, q, item, Ready, reason, actor, eventType, payload); err != nil {
		return db.WorkItem{}, err
	}
	item, err = q.DelayAvailability(ctx, db.DelayAvailabilityParams{ID: item.ID, DelaySeconds: s.policy.RetryDelay.Seconds()})
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("delay retry: %w", err)
	}
	return item, nil
}

// ReapReport counts what one reaper pass did.
type ReapReport struct {
	Expired int
	Stuck   int
	Gone    int
}

// Reap runs one pass of the background reaper: expired leases go back to
// ready (no attempt counted), stuck claims are revoked and counted as an
// attempt, and silent workers are marked gone. Rows are taken with SKIP
// LOCKED and re-checked under the lock, so replicas can reap concurrently.
func (s *Service) Reap(ctx context.Context) (ReapReport, error) {
	var report ReapReport
	system := Actor{Kind: ActorSystem, ID: "reaper"}
	err := s.inTx(ctx, func(q *db.Queries) error {
		ids, err := q.ExpiredLeases(ctx, reapBatch)
		if err != nil {
			return fmt.Errorf("find expired leases: %w", err)
		}
		for _, id := range ids {
			item, err := q.GetItemForUpdate(ctx, id)
			if err != nil {
				return fmt.Errorf("lock %s: %w", id, err)
			}
			if _, err := transition(ctx, q, item, Ready, "lease expired", system, EventLeaseExpired, map[string]any{"worker_id": item.ClaimedBy}); err != nil {
				return err
			}
		}
		report.Expired = len(ids)
		return nil
	})
	if err != nil {
		return report, err
	}
	err = s.inTx(ctx, func(q *db.Queries) error {
		ids, err := q.StuckItems(ctx, db.StuckItemsParams{StuckSeconds: s.policy.StuckAfter.Seconds(), MaxRows: reapBatch})
		if err != nil {
			return fmt.Errorf("find stuck work: %w", err)
		}
		for _, id := range ids {
			item, err := q.GetItemForUpdate(ctx, id)
			if err != nil {
				return fmt.Errorf("lock %s: %w", id, err)
			}
			reason := fmt.Sprintf("no stage report for %s", s.policy.StuckAfter)
			if _, err := s.failAttempt(ctx, q, item, reason, true, system, EventStuck); err != nil {
				return err
			}
		}
		report.Stuck = len(ids)
		return nil
	})
	if err != nil {
		return report, err
	}
	gone, err := db.New(s.pool).MarkSilentWorkersGone(ctx, s.policy.Lease.Seconds())
	if err != nil {
		return report, fmt.Errorf("mark silent workers gone: %w", err)
	}
	report.Gone = len(gone)
	return report, nil
}

func validateOutputs(outputs []OutputInput) error {
	for i, o := range outputs {
		if err := o.validate(); err != nil {
			return fmt.Errorf("outputs[%d]: %w", i, err)
		}
	}
	return nil
}
