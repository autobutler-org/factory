package work

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/jackc/pgx/v5"
)

// launcher is the actor recorded when the server plans an item for the
// sandboxes it starts.
var launcher = Actor{Kind: ActorSystem, ID: "launcher"}

// StartReady takes up to limit ready items, highest priority first, whose
// requirements are all among capabilities, plans each and moves it to
// in_progress, as an unrestricted claim would. It returns how many it took.
// A sandbox started for one chain never plans an item, so the server's
// launcher calls this before it starts sandboxes for the new chains.
func (s *Service) StartReady(ctx context.Context, capabilities []string, limit int) (int, error) {
	taken := 0
	for taken < limit {
		done := false
		err := s.inTx(ctx, func(q *db.Queries) error {
			id, err := q.PickNext(ctx, capabilities)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return fmt.Errorf("pick next item: %w", err)
			}
			item, err := q.GetItemForUpdate(ctx, id)
			if err != nil {
				return fmt.Errorf("lock %s: %w", id, err)
			}
			return s.start(ctx, q, item, launcher)
		})
		if err != nil || done {
			return taken, err
		}
		taken++
	}
	return taken, nil
}

// RunnableSandboxes returns the sandbox keys of the chains that have a step a
// sandboxed worker with capabilities could claim now and no step running,
// highest-priority item first. These are the chains that need a sandbox.
func (s *Service) RunnableSandboxes(ctx context.Context, capabilities []string) ([]string, error) {
	keys, err := db.New(s.pool).RunnableSandboxes(ctx, db.RunnableSandboxesParams{WaitKinds: plan.WaitKinds(), Capabilities: capabilities})
	if err != nil {
		return nil, fmt.Errorf("list runnable sandboxes: %w", err)
	}
	return keys, nil
}

// EndIdleSandbox calls end, which destroys the sandbox serving the chain
// with sandbox key key, only when no step of that chain is pending, ready or
// running, and reports whether it did. It holds the item's row lock while
// end runs, so no claim can hand the chain a step in the meantime: a sandbox
// whose worker holds a step is never ended. A key that names no work item
// is ended.
func (s *Service) EndIdleSandbox(ctx context.Context, key string, end func(context.Context) error) (bool, error) {
	itemID, _, _ := strings.Cut(key, "/")
	ended := false
	err := s.inTx(ctx, func(q *db.Queries) error {
		if uuidPattern.MatchString(itemID) {
			if _, err := q.GetItemForUpdate(ctx, itemID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock %s: %w", itemID, err)
			}
		}
		n, err := q.UnfinishedSandboxSteps(ctx, key)
		if err != nil {
			return fmt.Errorf("count unfinished steps of %s: %w", key, err)
		}
		if n > 0 {
			return nil
		}
		if err := end(ctx); err != nil {
			return err
		}
		ended = true
		return nil
	})
	return ended, err
}

// ClaimServerStep hands the server's own worker the next step that needs no
// sandbox and is not a wait step (merge), whatever the item requires. The
// rest of the protocol is the step protocol: the server reports the result
// with ReportStepResult, and an expired lease returns the step to ready.
func (s *Service) ClaimServerStep(ctx context.Context, workerID string) (claim StepClaim, ok bool, err error) {
	err = s.inTx(ctx, func(q *db.Queries) error {
		if _, err := activeWorker(ctx, q, workerID); err != nil {
			return err
		}
		for {
			row, err := q.PickServerStep(ctx, plan.WaitKinds())
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("pick server step: %w", err)
			}
			claim, err = s.claimStep(ctx, q, row.WorkItemID, row.ID, workerID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue // another claim took it first; pick again
			}
			ok = err == nil
			return err
		}
	})
	return claim, ok, err
}
