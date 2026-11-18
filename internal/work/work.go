// Package work is the work item service: it creates, updates and lists work
// items, records what they produce, and moves them through their state
// machine. It is the only code that changes a work item's state, and every
// state change writes a work_events row in the same transaction.
//
// It also stores each item's step graph (planned by internal/plan), leases
// steps to workers, schedules the steps that follow, and derives the item's
// state from its graph. See docs/architecture/graph.md.
//
// It does not speak HTTP or parse flags, and it does not decide who may call
// it; callers pass an Actor that is recorded on each event. See
// docs/architecture/work.md for the states and the rules. RepoToken holds the
// one rule about code host credentials: a token for a repository goes only
// to a worker whose live step claim, or local claim on the item, covers that
// repository (docs/architecture/codehost.md).
package work

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work states.
const (
	Design     = "design"
	Ready      = "ready"
	InProgress = "in_progress"
	InReview   = "in_review"
	Blocked    = "blocked"
	Failed     = "failed"
	Completed  = "completed"
	WontFix    = "wont_fix"
)

// Actor kinds recorded on events.
const (
	ActorAdapter = "adapter"
	ActorWorker  = "worker"
	ActorUser    = "user"
	ActorSystem  = "system"
)

// Output kinds and statuses.
const (
	KindPullRequest = "pull_request"
	KindCommit      = "commit"
	OutputOpen      = "open"
	OutputMerged    = "merged"
	OutputClosed    = "closed"
)

// Event types.
const (
	EventCreated        = "created"
	EventUpdated        = "updated"
	EventTransition     = "transition"
	EventOutputRecorded = "output_recorded"
	EventOutputsDone    = "outputs_done"
	EventDependencyAdd  = "dependency_added"
	EventDependencyDrop = "dependency_removed"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Size limits on what callers may store.
const (
	MaxTitleBytes = 1 << 10
	MaxBodyBytes  = 256 << 10
	maxListLimit  = 200
)

var (
	// ErrNotFound is returned for an item, dependency or output that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is returned for input that fails validation.
	ErrInvalid = errors.New("invalid input")
	// ErrIllegalTransition is returned for a move the state machine does not
	// allow. The error is a *TransitionError.
	ErrIllegalTransition = errors.New("illegal transition")
	// ErrUnfinishedDependencies is returned when work would start while an
	// item it is blocked by is neither completed nor wont_fix.
	ErrUnfinishedDependencies = errors.New("item has unfinished dependencies")
	// ErrOutputsPending is returned when completing an item that has no
	// landed output or still has an open one.
	ErrOutputsPending = errors.New("item cannot complete: it needs at least one merged output and no open ones")
	// ErrNoOutputs is returned when a worker says it is done but recorded
	// nothing that is open or merged.
	ErrNoOutputs = errors.New("item has no open or merged outputs")
	// ErrOutputConflict is returned when an output already belongs to another item.
	ErrOutputConflict = errors.New("output already recorded for another item")
)

// States lists every state in the order of the state table.
func States() []string {
	return []string{Design, Ready, InProgress, InReview, Blocked, Failed, Completed, WontFix}
}

// AllowedTargets returns the states an item in from may move to, or nil for
// an unknown state.
func AllowedTargets(from string) []string {
	switch from {
	case Design:
		return []string{Ready, Blocked, WontFix}
	case Ready:
		return []string{InProgress, Design, Blocked, WontFix}
	case InProgress:
		return []string{InReview, Ready, Blocked, Failed, Completed}
	case InReview:
		return []string{InProgress, Completed, Blocked, Failed, WontFix}
	case Blocked:
		return []string{Design, Ready, WontFix}
	case Failed:
		return []string{Ready, WontFix}
	case Completed, WontFix:
		return []string{Design, Ready}
	}
	return nil
}

// TransitionError says why a move is illegal: where the item is and where it
// could go instead.
type TransitionError struct {
	From    string
	To      string
	Allowed []string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal transition from %s to %s (allowed: %v)", e.From, e.To, e.Allowed)
}

func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// Actor is who caused a change. It is recorded on every event.
type Actor struct {
	Kind string
	ID   string
}

// Service is the work item service.
type Service struct {
	pool   *pgxpool.Pool
	policy lease.Policy
}

// New returns a Service backed by pool with lease.DefaultPolicy.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, policy: lease.DefaultPolicy()}
}

// WithPolicy returns a copy of s that leases work with p.
func (s *Service) WithPolicy(p lease.Policy) *Service {
	return &Service{pool: s.pool, policy: p}
}

// NewItem is a work item to create or upsert. Source and ExternalID are both
// set for work that lives in a tracker, or both empty for work that exists
// only in the factory; such work may carry an IdempotencyKey so a retried
// request finds the item the first one created. State is design or ready;
// empty means ready. BlockedBy names items by ID or as "source:external_id".
type NewItem struct {
	Source         string
	ExternalID     string
	ExternalURL    string
	IdempotencyKey string
	Title          string
	Body           string
	Repos          []string
	State          string
	Priority       int32
	BlockedBy      []string
	// Requires lists the capabilities (harnesses or labels) a worker needs
	// to be handed this item.
	Requires []string
}

// UpsertResult is the item an Upsert left behind and what it did.
type UpsertResult struct {
	Item    db.WorkItem
	Created bool
	// Changed is false when an existing item was left as it was; Reason says why.
	Changed bool
	Reason  string
}

func (in NewItem) validate() error {
	switch {
	case in.Title == "":
		return fmt.Errorf("%w: title is required", ErrInvalid)
	case len(in.Title) > MaxTitleBytes:
		return fmt.Errorf("%w: title is longer than %d bytes", ErrInvalid, MaxTitleBytes)
	case len(in.Body) > MaxBodyBytes:
		return fmt.Errorf("%w: body is longer than %d bytes", ErrInvalid, MaxBodyBytes)
	case (in.Source == "") != (in.ExternalID == ""):
		return fmt.Errorf("%w: source and external_id go together: give both or neither", ErrInvalid)
	case in.State != "" && in.State != Design && in.State != Ready:
		return fmt.Errorf("%w: state must be design or ready, not %q", ErrInvalid, in.State)
	case slices.Contains(in.Repos, ""):
		return fmt.Errorf("%w: repos must not contain an empty string", ErrInvalid)
	case slices.Contains(in.Requires, ""):
		return fmt.Errorf("%w: requires must not contain an empty string", ErrInvalid)
	}
	return nil
}

// Upsert creates an item, or, when one with the same (source, external_id) or
// idempotency key exists, refreshes its title, body and URL while it is in
// design or ready. It never changes an existing item's state.
func (s *Service) Upsert(ctx context.Context, in NewItem, actor Actor) (UpsertResult, error) {
	if err := in.validate(); err != nil {
		return UpsertResult{}, err
	}
	state := in.State
	if state == "" {
		state = Ready
	}
	repos := in.Repos
	if repos == nil {
		repos = []string{}
	}
	requires := in.Requires
	if requires == nil {
		requires = []string{}
	}
	key := nilIfEmpty(in.IdempotencyKey)
	if in.Source != "" {
		key = nil // the external identity already dedupes
	}
	var res UpsertResult
	err := s.inTx(ctx, func(q *db.Queries) error {
		blockedBy := make([]string, 0, len(in.BlockedBy))
		for _, ref := range in.BlockedBy {
			id, err := resolve(ctx, q, ref)
			if err != nil {
				return err
			}
			blockedBy = append(blockedBy, id)
		}
		item, err := q.InsertItem(ctx, db.InsertItemParams{
			Source: nilIfEmpty(in.Source), ExternalID: nilIfEmpty(in.ExternalID), ExternalUrl: nilIfEmpty(in.ExternalURL),
			Title: in.Title, Body: in.Body, Repos: repos, State: state, Priority: in.Priority, IdempotencyKey: key, Requires: requires,
		})
		switch {
		case err == nil:
			res = UpsertResult{Item: item, Created: true, Changed: true}
			if _, err := writeEvent(ctx, q, item.ID, actor, EventCreated, "", state, map[string]any{"title": in.Title}); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("insert work item: %w", err)
		default:
			// ON CONFLICT DO NOTHING: the item exists.
			if in.Source != "" {
				item, err = q.GetItemBySourceForUpdate(ctx, db.GetItemBySourceForUpdateParams{Source: &in.Source, ExternalID: &in.ExternalID})
			} else {
				item, err = q.GetItemByIdempotencyKeyForUpdate(ctx, key)
			}
			if err != nil {
				return fmt.Errorf("read existing work item: %w", err)
			}
			if res, err = refresh(ctx, q, item, in, actor); err != nil {
				return err
			}
		}
		if res.Item.State != Design && res.Item.State != Ready {
			return nil
		}
		for _, dep := range blockedBy {
			if err := addDependency(ctx, q, res.Item.ID, dep, actor); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func refresh(ctx context.Context, q *db.Queries, item db.WorkItem, in NewItem, actor Actor) (UpsertResult, error) {
	if item.State != Design && item.State != Ready {
		return UpsertResult{Item: item, Reason: "item is " + item.State + "; title and body are frozen once work has started"}, nil
	}
	if item.Title == in.Title && item.Body == in.Body && (in.ExternalURL == "" || deref(item.ExternalUrl) == in.ExternalURL) {
		return UpsertResult{Item: item, Reason: "nothing to change"}, nil
	}
	item, err := q.UpdateItemContent(ctx, db.UpdateItemContentParams{ID: item.ID, Title: in.Title, Body: in.Body, ExternalUrl: nilIfEmpty(in.ExternalURL)})
	if err != nil {
		return UpsertResult{}, fmt.Errorf("update work item: %w", err)
	}
	_, err = writeEvent(ctx, q, item.ID, actor, EventUpdated, "", "", map[string]any{"title": in.Title})
	return UpsertResult{Item: item, Changed: true}, err
}

// resolve turns a reference to an item, its ID or "source:external_id", into
// its ID.
func resolve(ctx context.Context, q *db.Queries, ref string) (string, error) {
	if uuidPattern.MatchString(ref) {
		item, err := q.GetItem(ctx, ref)
		return item.ID, notFound(err, "work item "+ref)
	}
	source, externalID, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("work item %q: %w", ref, ErrNotFound)
	}
	item, err := q.GetItemBySource(ctx, db.GetItemBySourceParams{Source: &source, ExternalID: &externalID})
	if err != nil {
		return "", notFound(err, "work item "+ref)
	}
	return item.ID, nil
}

// Get returns one item.
func (s *Service) Get(ctx context.Context, id string) (db.WorkItem, error) {
	item, err := db.New(s.pool).GetItem(ctx, id)
	return item, notFound(err, "work item "+id)
}

// Filter narrows List. Empty fields do not filter. Cursor is the value List
// returned for the previous page.
type Filter struct {
	State     string
	Source    string
	Repo      string
	ClaimedBy string
	Limit     int
	Cursor    string
}

type cursor struct {
	Priority  int32     `json:"p"`
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// List returns items by priority, highest first, then oldest first, and the
// cursor for the next page, which is empty on the last page.
func (s *Service) List(ctx context.Context, f Filter) ([]db.WorkItem, string, error) {
	if f.State != "" && AllowedTargets(f.State) == nil {
		return nil, "", fmt.Errorf("%w: unknown state %q", ErrInvalid, f.State)
	}
	if f.ClaimedBy != "" && !uuidPattern.MatchString(f.ClaimedBy) {
		return nil, "", fmt.Errorf("%w: claimed_by must be a worker id", ErrInvalid)
	}
	limit := f.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	params := db.ListItemsParams{State: nilIfEmpty(f.State), Source: nilIfEmpty(f.Source), Repo: nilIfEmpty(f.Repo), ClaimedBy: nilIfEmpty(f.ClaimedBy), MaxRows: int32(limit + 1)}
	if f.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		var c cursor
		if err == nil {
			err = json.Unmarshal(raw, &c)
		}
		if err != nil || c.ID == "" {
			return nil, "", fmt.Errorf("%w: cursor is not one this API returned", ErrInvalid)
		}
		params.AfterID, params.AfterPriority, params.AfterCreatedAt = &c.ID, &c.Priority, &c.CreatedAt
	}
	items, err := db.New(s.pool).ListItems(ctx, params)
	if err != nil {
		return nil, "", fmt.Errorf("list work items: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	last := items[limit-1]
	raw, err := json.Marshal(cursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil {
		return nil, "", fmt.Errorf("encode cursor: %w", err)
	}
	return items, base64.RawURLEncoding.EncodeToString(raw), nil
}

// Patch changes the fields that are set.
type Patch struct {
	Title    *string
	Body     *string
	Priority *int32
}

// Patch edits an item's title, body or priority. It does not change state.
func (s *Service) Patch(ctx context.Context, id string, p Patch, actor Actor) (db.WorkItem, error) {
	switch {
	case p.Title != nil && (*p.Title == "" || len(*p.Title) > MaxTitleBytes):
		return db.WorkItem{}, fmt.Errorf("%w: title must be 1 to %d bytes", ErrInvalid, MaxTitleBytes)
	case p.Body != nil && len(*p.Body) > MaxBodyBytes:
		return db.WorkItem{}, fmt.Errorf("%w: body is longer than %d bytes", ErrInvalid, MaxBodyBytes)
	}
	var item db.WorkItem
	err := s.inTx(ctx, func(q *db.Queries) error {
		var err error
		item, err = q.PatchItem(ctx, db.PatchItemParams{ID: id, Title: p.Title, Body: p.Body, Priority: p.Priority})
		if err != nil {
			return notFound(err, "work item "+id)
		}
		_, err = writeEvent(ctx, q, id, actor, EventUpdated, "", "", map[string]any{"title": p.Title, "body_changed": p.Body != nil, "priority": p.Priority})
		return err
	})
	return item, err
}

// Transition moves an item to another state, checking the state table, the
// dependency rule (no start while a dependency is unfinished) and the
// completion rule (at least one merged output and none open).
func (s *Service) Transition(ctx context.Context, id, to, reason string, actor Actor) (db.WorkItem, error) {
	var item db.WorkItem
	err := s.inTx(ctx, func(q *db.Queries) error {
		current, err := q.GetItemForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "work item "+id)
		}
		item, err = transition(ctx, q, current, to, reason, actor, EventTransition, nil)
		return err
	})
	return item, err
}

// transition is the one place an item's state is written. The caller holds
// the row lock on item.
func transition(ctx context.Context, q *db.Queries, item db.WorkItem, to, reason string, actor Actor, eventType string, payload map[string]any) (db.WorkItem, error) {
	allowed := AllowedTargets(item.State)
	if !slices.Contains(allowed, to) {
		return db.WorkItem{}, &TransitionError{From: item.State, To: to, Allowed: allowed}
	}
	if item.State == Ready && to == InProgress {
		n, err := q.CountUnfinishedDependencies(ctx, item.ID)
		if err != nil {
			return db.WorkItem{}, fmt.Errorf("count dependencies: %w", err)
		}
		if n > 0 {
			return db.WorkItem{}, fmt.Errorf("%d unfinished: %w", n, ErrUnfinishedDependencies)
		}
	}
	if to == Completed {
		counts, err := q.CountOutputs(ctx, item.ID)
		if err != nil {
			return db.WorkItem{}, fmt.Errorf("count outputs: %w", err)
		}
		if counts.Open > 0 || counts.Merged == 0 {
			return db.WorkItem{}, fmt.Errorf("%d open, %d merged: %w", counts.Open, counts.Merged, ErrOutputsPending)
		}
	}
	// Work that starts again may produce more, so it is no longer done.
	outputsDone := item.OutputsDone && to != Design && to != Ready && to != InProgress
	updated, err := q.UpdateItemState(ctx, db.UpdateItemStateParams{ID: item.ID, State: to, StateReason: reason, OutputsDone: outputsDone})
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("update state: %w", err)
	}
	// Leaving the states in which a graph runs stops it.
	if !active(to) {
		if _, err := q.CancelSteps(ctx, item.ID); err != nil {
			return db.WorkItem{}, fmt.Errorf("cancel steps: %w", err)
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if _, err := writeEvent(ctx, q, item.ID, actor, eventType, item.State, to, payload); err != nil {
		return db.WorkItem{}, err
	}
	return updated, nil
}

// Events returns an item's history, oldest first.
func (s *Service) Events(ctx context.Context, id string) ([]db.WorkEvent, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	events, err := db.New(s.pool).ListEvents(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// OutputInput is a pull request or commit a work item produced. A commit is
// merged from the start; its Status is ignored.
type OutputInput struct {
	Repo   string `json:"repo"`
	Kind   string `json:"kind"`
	Branch string `json:"branch"`
	URL    string `json:"url"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

func (o OutputInput) validate() error {
	switch {
	case o.Repo == "":
		return fmt.Errorf("%w: output repo is required", ErrInvalid)
	case o.Kind == KindPullRequest && o.URL == "":
		return fmt.Errorf("%w: a pull_request output needs its url", ErrInvalid)
	case o.Kind == KindPullRequest && o.Status != OutputOpen && o.Status != OutputMerged && o.Status != OutputClosed:
		return fmt.Errorf("%w: output status must be open, merged or closed", ErrInvalid)
	case o.Kind == KindCommit && o.SHA == "":
		return fmt.Errorf("%w: a commit output needs its sha", ErrInvalid)
	case o.Kind != KindPullRequest && o.Kind != KindCommit:
		return fmt.Errorf("%w: output kind must be pull_request or commit", ErrInvalid)
	}
	return nil
}

// RecordOutput records or updates an output of an item. Once the worker has
// said it is done, an update that lands the last open output completes the
// item.
func (s *Service) RecordOutput(ctx context.Context, id string, o OutputInput, actor Actor) (db.WorkOutput, error) {
	if err := o.validate(); err != nil {
		return db.WorkOutput{}, err
	}
	var out db.WorkOutput
	err := s.inTx(ctx, func(q *db.Queries) error {
		item, err := q.GetItemForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "work item "+id)
		}
		out, err = recordOutput(ctx, q, item, o, actor)
		if err != nil {
			return err
		}
		_, err = settle(ctx, q, item, actor)
		return err
	})
	return out, err
}

// recordOutput writes one output; the caller holds the row lock on item.
func recordOutput(ctx context.Context, q *db.Queries, item db.WorkItem, o OutputInput, actor Actor) (db.WorkOutput, error) {
	var out db.WorkOutput
	var err error
	if o.Kind == KindPullRequest {
		out, err = q.UpsertPullRequest(ctx, db.UpsertPullRequestParams{WorkItemID: item.ID, Repo: o.Repo, Branch: o.Branch, Url: o.URL, Sha: o.SHA, Status: o.Status})
	} else {
		out, err = q.UpsertCommit(ctx, db.UpsertCommitParams{WorkItemID: item.ID, Repo: o.Repo, Branch: o.Branch, Url: o.URL, Sha: o.SHA})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkOutput{}, ErrOutputConflict
	}
	if err != nil {
		return db.WorkOutput{}, fmt.Errorf("record output: %w", err)
	}
	_, err = writeEvent(ctx, q, item.ID, actor, EventOutputRecorded, "", "", map[string]any{
		"output_id": out.ID, "repo": out.Repo, "kind": out.Kind, "url": out.Url, "sha": out.Sha, "status": out.Status,
	})
	return out, err
}

// FinishOutputs records that whoever is working the item has nothing more to
// produce. The item moves to in_review while an output is open, or to
// completed when every output has landed.
func (s *Service) FinishOutputs(ctx context.Context, id string, actor Actor) (db.WorkItem, error) {
	var item db.WorkItem
	err := s.inTx(ctx, func(q *db.Queries) error {
		current, err := q.GetItemForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "work item "+id)
		}
		item, err = finishOutputs(ctx, q, current, actor)
		return err
	})
	return item, err
}

// finishOutputs is FinishOutputs inside a transaction that holds the row lock.
func finishOutputs(ctx context.Context, q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
	if item.State != InProgress && item.State != InReview {
		return db.WorkItem{}, &TransitionError{From: item.State, To: InReview, Allowed: AllowedTargets(item.State)}
	}
	counts, err := q.CountOutputs(ctx, item.ID)
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("count outputs: %w", err)
	}
	if counts.Open == 0 && counts.Merged == 0 {
		return db.WorkItem{}, ErrNoOutputs
	}
	item, err = q.SetOutputsDone(ctx, item.ID)
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("mark outputs done: %w", err)
	}
	if _, err := writeEvent(ctx, q, item.ID, actor, EventOutputsDone, "", "", map[string]any{"open": counts.Open, "merged": counts.Merged}); err != nil {
		return db.WorkItem{}, err
	}
	return settle(ctx, q, item, actor)
}

// settle applies the output rules to an item whose worker has finished: an
// open output means in_review; every output landed means completed.
func settle(ctx context.Context, q *db.Queries, item db.WorkItem, actor Actor) (db.WorkItem, error) {
	if !item.OutputsDone {
		return item, nil
	}
	counts, err := q.CountOutputs(ctx, item.ID)
	if err != nil {
		return db.WorkItem{}, fmt.Errorf("count outputs: %w", err)
	}
	switch {
	case counts.Open > 0 && item.State == InProgress:
		return transition(ctx, q, item, InReview, "", actor, EventTransition, nil)
	case counts.Open == 0 && counts.Merged > 0 && (item.State == InProgress || item.State == InReview):
		return transition(ctx, q, item, Completed, "", actor, EventTransition, nil)
	}
	return item, nil
}

// Outputs returns what an item produced, oldest first.
func (s *Service) Outputs(ctx context.Context, id string) ([]db.WorkOutput, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	outputs, err := db.New(s.pool).ListOutputs(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	return outputs, nil
}

// AddDependency records that id is blocked by blockedBy. Adding it twice is a
// no-op.
//
// ponytail: no cycle check; two items blocked by each other are never handed
// out. Add a recursive check here if cycles show up in practice.
func (s *Service) AddDependency(ctx context.Context, id, blockedBy string, actor Actor) error {
	return s.inTx(ctx, func(q *db.Queries) error { return addDependency(ctx, q, id, blockedBy, actor) })
}

func addDependency(ctx context.Context, q *db.Queries, id, blockedBy string, actor Actor) error {
	if id == blockedBy {
		return fmt.Errorf("%w: an item cannot be blocked by itself", ErrInvalid)
	}
	for _, each := range []string{id, blockedBy} {
		if _, err := q.GetItem(ctx, each); err != nil {
			return notFound(err, "work item "+each)
		}
	}
	n, err := q.AddDependency(ctx, db.AddDependencyParams{WorkItemID: id, BlockedByID: blockedBy})
	if err != nil || n == 0 {
		return err
	}
	_, err = writeEvent(ctx, q, id, actor, EventDependencyAdd, "", "", map[string]any{"blocked_by_id": blockedBy})
	return err
}

// RemoveDependency drops a dependency.
func (s *Service) RemoveDependency(ctx context.Context, id, blockedBy string, actor Actor) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		n, err := q.RemoveDependency(ctx, db.RemoveDependencyParams{WorkItemID: id, BlockedByID: blockedBy})
		if err != nil {
			return notFound(err, "dependency")
		}
		if n == 0 {
			return fmt.Errorf("dependency of %s on %s: %w", id, blockedBy, ErrNotFound)
		}
		_, err = writeEvent(ctx, q, id, actor, EventDependencyDrop, "", "", map[string]any{"blocked_by_id": blockedBy})
		return err
	})
}

// Dependencies lists the items id is blocked by.
func (s *Service) Dependencies(ctx context.Context, id string) ([]string, error) {
	deps, err := db.New(s.pool).ListDependencies(ctx, id)
	return deps, notFound(err, "work item "+id)
}

func (s *Service) inTx(ctx context.Context, f func(*db.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return f(db.New(tx)) })
}

func writeEvent(ctx context.Context, q *db.Queries, id string, actor Actor, eventType, from, to string, payload map[string]any) (db.WorkEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return db.WorkEvent{}, fmt.Errorf("encode event payload: %w", err)
	}
	e, err := q.InsertEvent(ctx, db.InsertEventParams{
		WorkItemID: id, ActorKind: actor.Kind, ActorID: actor.ID, Type: eventType,
		FromState: nilIfEmpty(from), ToState: nilIfEmpty(to), Payload: raw,
	})
	if err != nil {
		return db.WorkEvent{}, fmt.Errorf("write %s event: %w", eventType, err)
	}
	return e, nil
}

// notFound maps a missing row, a malformed UUID or a dangling foreign key to
// ErrNotFound and wraps anything else.
func notFound(err error, what string) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows),
		errors.As(err, &pgErr) && (pgErr.Code == "22P02" || pgErr.Code == "23503"):
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
