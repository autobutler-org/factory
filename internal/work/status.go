package work

import (
	"context"
	"fmt"

	"github.com/autobutler-org/factory/internal/db"
)

// recentFailures is how many failed items Status lists.
const recentFailures = 5

// Status is the factory at a glance: how much work is in each state, the
// workers by state, the item that has waited in ready longest, and the
// latest failures.
type Status struct {
	// Counts has every state, zero included.
	Counts map[string]int64 `json:"counts"`
	// Workers counts registered workers by state (active, draining, gone).
	Workers map[string]int64 `json:"workers"`
	// OldestReady is nil when nothing is ready.
	OldestReady *db.WorkItem `json:"oldest_ready"`
	// RecentFailures are the latest failed items, newest first.
	RecentFailures []db.WorkItem `json:"recent_failures"`
}

// Status reads the factory's status.
//
// ponytail: counts scan work_items; add a state index or a running tally if
// the table grows past what a scan answers quickly.
func (s *Service) Status(ctx context.Context) (Status, error) {
	q := db.New(s.pool)
	st := Status{Counts: map[string]int64{}, Workers: map[string]int64{}, RecentFailures: []db.WorkItem{}}
	for _, state := range States() {
		st.Counts[state] = 0
	}
	counts, err := q.CountItemsByState(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("count items by state: %w", err)
	}
	for _, c := range counts {
		st.Counts[c.State] = c.Items
	}
	oldest, err := q.OldestReadyItem(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("find the oldest ready item: %w", err)
	}
	if len(oldest) > 0 {
		st.OldestReady = &oldest[0]
	}
	failed, err := q.RecentFailedItems(ctx, recentFailures)
	if err != nil {
		return Status{}, fmt.Errorf("list recent failures: %w", err)
	}
	st.RecentFailures = append(st.RecentFailures, failed...)
	workers, err := q.ListWorkers(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("list workers: %w", err)
	}
	for _, w := range workers {
		st.Workers[w.State]++
	}
	return st, nil
}
