package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/jackc/pgx/v5"
)

// Graph states.
const (
	GraphCurrent    = "current"
	GraphSuperseded = "superseded"
	GraphHandedOver = "handed_over"
)

// Step outcomes a worker or an adapter reports.
const (
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepBlocked   = "blocked"
)

// Event types written by the step graph. Each lands on the item's history
// with the step's id and key in the payload.
const (
	EventPlanned          = "planned"
	EventStepClaimed      = "step_claimed"
	EventStepReleased     = "step_released"
	EventStepStage        = "step_stage_reported"
	EventStepResult       = "step_result_reported"
	EventStepCompleted    = "step_completed"
	EventStepLeaseExpired = "step_lease_expired"
	EventStepStuck        = "step_stuck"
)

var (
	// ErrNotWaiting is returned when completing a step that is not a wait
	// step waiting for its event.
	ErrNotWaiting = errors.New("step is not a wait step that is waiting")
	// ErrNotReplannable is returned when replanning an item that is not being
	// worked. A ready item is planned when it is taken.
	ErrNotReplannable = errors.New("only an in_progress or in_review item can be replanned")
)

// StepClaim is what a worker gets when it claims a step: the step, its item,
// and what it needs to resume: the results of the steps that have succeeded
// in the graph, by key, and the item's outputs so far.
type StepClaim struct {
	Step    db.WorkStep                `json:"step"`
	Item    db.WorkItem                `json:"item"`
	Results map[string]json.RawMessage `json:"results"`
	Outputs []db.WorkOutput            `json:"outputs"`
}

// StepResult is how a step ended. Outcome is succeeded, failed or blocked;
// Retryable applies to failed only. Result is the kind's result document.
type StepResult struct {
	Outcome   string
	Result    json.RawMessage
	Reason    string
	Retryable bool
	Outputs   []OutputInput
}

func (r StepResult) validate() error {
	if r.Outcome != StepSucceeded && r.Outcome != StepFailed && r.Outcome != StepBlocked {
		return fmt.Errorf("%w: outcome must be succeeded, failed or blocked", ErrInvalid)
	}
	if len(r.Result) > 0 && !json.Valid(r.Result) {
		return fmt.Errorf("%w: result is not JSON", ErrInvalid)
	}
	return validateOutputs(r.Outputs)
}

// GraphView is a graph with its steps, in plan order, its edges, and the
// gate outputs kept for its steps (PutStepOutput).
type GraphView struct {
	Graph       db.WorkGraph      `json:"graph"`
	Steps       []db.WorkStep     `json:"steps"`
	Edges       []db.WorkStepEdge `json:"edges"`
	StepOutputs []StepOutputInfo  `json:"step_outputs"`
}

// active says whether an item is being worked by its graph.
func active(state string) bool { return state == InProgress || state == InReview }

// ClaimStep hands the worker the next step it may run: the highest-priority
// item first, never a wait step, never a step whose sandbox already has one
// running. With sandboxKey set, only that sandbox's steps, and the claim
// waits for the chain's item lock instead of skipping the item, so ok false
// means the chain has nothing ready. When no planned
// step is ready, it takes the next ready item, plans it and moves it to
// in_progress. ok is false when there is nothing to hand out.
func (s *Service) ClaimStep(ctx context.Context, workerID, sandboxKey string) (claim StepClaim, ok bool, err error) {
	err = s.inTx(ctx, func(q *db.Queries) error {
		w, err := activeWorker(ctx, q, workerID)
		if err != nil {
			return err
		}
		var caps Capabilities
		if err := json.Unmarshal(w.Capabilities, &caps); err != nil {
			return fmt.Errorf("decode capabilities of worker %s: %w", workerID, err)
		}
		// A sandbox's chain belongs to one item. Wait for that item's lock
		// rather than let PickStep skip it: the launcher locks it on every
		// pass, and a skipped item would look like a finished chain, so the
		// worker would exit with a step still to run.
		if itemID, _, _ := strings.Cut(sandboxKey, "/"); sandboxKey != "" && uuidPattern.MatchString(itemID) {
			if _, err := q.GetItemForUpdate(ctx, itemID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock %s: %w", itemID, err)
			}
		}
		// Each pass either claims, finds nothing, moves one item out of
		// ready, or puts one off until the code host answers, so the loop
		// ends.
		deferred := []string{}
		for {
			row, err := q.PickStep(ctx, db.PickStepParams{WaitKinds: plan.WaitKinds(), Capabilities: caps.all(), SandboxKey: nilIfEmpty(sandboxKey)})
			if err == nil {
				claim, err = s.claimStep(ctx, q, row.WorkItemID, row.ID, workerID)
				if errors.Is(err, pgx.ErrNoRows) {
					continue // another claim took it first; pick again
				}
				ok = err == nil
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("pick step: %w", err)
			}
			if sandboxKey != "" {
				return nil
			}
			id, err := q.PickNext(ctx, db.PickNextParams{Skip: deferred, Capabilities: caps.all()})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("pick next item: %w", err)
			}
			item, err := q.GetItemForUpdate(ctx, id)
			if err != nil {
				return fmt.Errorf("lock %s: %w", id, err)
			}
			putOff, err := s.start(ctx, q, item, Actor{Kind: ActorWorker, ID: workerID})
			if err != nil {
				return err
			}
			if putOff {
				deferred = append(deferred, id)
			}
		}
	})
	return claim, ok, err
}

// start takes an item from ready: it reads its repositories' configs, plans
// a new graph and moves the item to in_progress. It blocks the item when a
// config is unusable or the item cannot be planned. When the code host
// cannot be read it leaves the item ready, available again after the retry
// delay, and reports put off.
//
// ponytail: the configs are read over the network while the item's row lock
// is held, one repository after another; read them before the transaction,
// or cache them, if planning ever waits on that.
func (s *Service) start(ctx context.Context, q *db.Queries, item db.WorkItem, actor Actor) (putOff bool, err error) {
	configs, reads, err := s.readConfigs(ctx, item.Repos)
	if errors.Is(err, ErrConfigUnavailable) {
		if _, err := q.DelayAvailability(ctx, db.DelayAvailabilityParams{ID: item.ID, DelaySeconds: s.policy.RetryDelay.Seconds()}); err != nil {
			return false, fmt.Errorf("put off %s: %w", item.ID, err)
		}
		_, err = writeEvent(ctx, q, item.ID, planner, EventPlanDeferred, "", "", map[string]any{"reason": err.Error()})
		return true, err
	}
	var g plan.Graph
	if err == nil {
		g, err = plan.Plan(plan.Item{ID: item.ID, Title: item.Title, Repos: item.Repos}, configs, plan.DefaultTemplate())
	}
	if err != nil {
		_, err = transition(ctx, q, item, Blocked, "cannot plan: "+err.Error(), planner, EventTransition, nil)
		return false, err
	}
	if _, err := transition(ctx, q, item, InProgress, "", actor, EventTransition, nil); err != nil {
		return false, err
	}
	_, err = writeGraph(ctx, q, item.ID, g, reads, actor)
	return false, err
}

// planner is the actor recorded when planning blocks or puts off an item.
var planner = Actor{Kind: ActorSystem, ID: "planner"}

// writeGraph replaces the item's current graph with g: the old one's
// unfinished steps are canceled and it is marked superseded. The caller holds
// the item's row lock.
func writeGraph(ctx context.Context, q *db.Queries, itemID string, g plan.Graph, reads []configRead, actor Actor) (db.WorkGraph, error) {
	if err := g.Validate(); err != nil {
		return db.WorkGraph{}, err
	}
	if err := endGraph(ctx, q, itemID, GraphSuperseded); err != nil {
		return db.WorkGraph{}, err
	}
	graph, err := q.InsertGraph(ctx, db.InsertGraphParams{WorkItemID: itemID, Template: g.Template, InputsHash: g.InputsHash})
	if err != nil {
		return db.WorkGraph{}, fmt.Errorf("insert graph: %w", err)
	}
	ids := make(map[string]string, len(g.Steps))
	for i, st := range g.Steps {
		params, err := json.Marshal(st.Params)
		if err != nil {
			return db.WorkGraph{}, fmt.Errorf("encode params of %s: %w", st.Key, err)
		}
		id, err := q.InsertStep(ctx, db.InsertStepParams{
			GraphID: graph.ID, Position: int32(i), Key: st.Key, Kind: st.Kind, Params: params, SandboxKey: st.SandboxKey, Condition: st.Condition,
		})
		if err != nil {
			return db.WorkGraph{}, fmt.Errorf("insert step %s: %w", st.Key, err)
		}
		ids[st.Key] = id
	}
	for _, st := range g.Steps {
		for _, d := range st.DependsOn {
			if err := q.InsertEdge(ctx, db.InsertEdgeParams{StepID: ids[st.Key], DependsOnID: ids[d]}); err != nil {
				return db.WorkGraph{}, fmt.Errorf("insert edge %s -> %s: %w", st.Key, d, err)
			}
		}
	}
	if _, err := writeEvent(ctx, q, itemID, actor, EventPlanned, "", "", map[string]any{
		"graph_id": graph.ID, "version": graph.Version, "template": graph.Template, "inputs_hash": graph.InputsHash, "steps": len(g.Steps),
		"configs": reads,
	}); err != nil {
		return db.WorkGraph{}, err
	}
	return graph, schedule(ctx, q, graph.ID)
}

// endGraph cancels the unfinished steps of the item's current graph and
// marks it superseded or handed_over.
func endGraph(ctx context.Context, q *db.Queries, itemID, state string) error {
	if _, err := q.CancelSteps(ctx, itemID); err != nil {
		return fmt.Errorf("cancel steps: %w", err)
	}
	if _, err := q.EndCurrentGraph(ctx, db.EndCurrentGraphParams{WorkItemID: itemID, State: state}); err != nil {
		return fmt.Errorf("end graph: %w", err)
	}
	return nil
}

// schedule applies plan.Next to a stored graph: pending steps whose
// dependencies are done become ready or skipped. It runs in the transaction
// that changed a step, so the chain moves on without a polling loop.
func schedule(ctx context.Context, q *db.Queries, graphID string) error {
	steps, err := q.ListSteps(ctx, graphID)
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}
	edges, err := q.ListEdges(ctx, graphID)
	if err != nil {
		return fmt.Errorf("list edges: %w", err)
	}
	keys := make(map[string]string, len(steps))
	for _, st := range steps {
		keys[st.ID] = st.Key
	}
	deps := map[string][]string{}
	for _, e := range edges {
		deps[e.StepID] = append(deps[e.StepID], keys[e.DependsOnID])
	}
	g := plan.Graph{Steps: make([]plan.Step, len(steps))}
	status := make(map[string]plan.Status, len(steps))
	for i, st := range steps {
		g.Steps[i] = plan.Step{Key: st.Key, Kind: st.Kind, Condition: st.Condition, DependsOn: deps[st.ID]}
		status[st.Key] = plan.Status{State: st.State, Verdict: plan.Verdict(st.Result)}
	}
	changes := plan.Next(g, status)
	for _, st := range steps {
		if next, ok := changes[st.Key]; ok {
			if err := q.SetStepState(ctx, db.SetStepStateParams{ID: st.ID, State: next}); err != nil {
				return fmt.Errorf("set %s %s: %w", st.Key, next, err)
			}
		}
	}
	return nil
}

func (s *Service) claimStep(ctx context.Context, q *db.Queries, itemID, stepID, workerID string) (StepClaim, error) {
	step, err := q.SetStepClaim(ctx, db.SetStepClaimParams{ID: stepID, WorkerID: &workerID, LeaseSeconds: s.policy.Lease.Seconds()})
	if err != nil {
		return StepClaim{}, fmt.Errorf("claim step: %w", err) // pgx.ErrNoRows: no longer claimable
	}
	if _, err := writeEvent(ctx, q, itemID, Actor{Kind: ActorWorker, ID: workerID}, EventStepClaimed, "", "", stepPayload(step, map[string]any{"worker_id": workerID})); err != nil {
		return StepClaim{}, err
	}
	item, err := q.GetItem(ctx, itemID)
	if err != nil {
		return StepClaim{}, fmt.Errorf("read item: %w", err)
	}
	steps, err := q.ListSteps(ctx, step.GraphID)
	if err != nil {
		return StepClaim{}, fmt.Errorf("list steps: %w", err)
	}
	results := map[string]json.RawMessage{}
	for _, st := range steps {
		if st.State == plan.Succeeded {
			results[st.Key] = st.Result
		}
	}
	outputs, err := q.ListOutputs(ctx, itemID)
	if err != nil {
		return StepClaim{}, fmt.Errorf("list outputs: %w", err)
	}
	return StepClaim{Step: step, Item: item, Results: results, Outputs: outputs}, nil
}

func stepPayload(step db.WorkStep, extra map[string]any) map[string]any {
	payload := map[string]any{"step_id": step.ID, "key": step.Key, "kind": step.Kind}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// lockStep locks a step's item, then the step: the lock order every step
// change follows.
func lockStep(ctx context.Context, q *db.Queries, stepID string) (db.WorkItem, db.WorkStep, bool, error) {
	si, err := q.StepItem(ctx, stepID)
	if err != nil {
		return db.WorkItem{}, db.WorkStep{}, false, notFound(err, "step "+stepID)
	}
	item, err := q.GetItemForUpdate(ctx, si.WorkItemID)
	if err != nil {
		return db.WorkItem{}, db.WorkStep{}, false, fmt.Errorf("lock item: %w", err)
	}
	step, err := q.GetStepForUpdate(ctx, stepID)
	if err != nil {
		return db.WorkItem{}, db.WorkStep{}, false, fmt.Errorf("lock step: %w", err)
	}
	return item, step, si.Current, nil
}

// asStepClaimant runs f only if workerID holds a live lease on the step.
func (s *Service) asStepClaimant(ctx context.Context, stepID, workerID string, f func(*db.Queries, db.WorkItem, db.WorkStep, Actor) (db.WorkStep, error)) (db.WorkStep, error) {
	var out db.WorkStep
	err := s.inTx(ctx, func(q *db.Queries) error {
		item, step, _, err := lockStep(ctx, q, stepID)
		if err != nil {
			return err
		}
		live, err := q.StepLeaseLive(ctx, stepID)
		if err != nil {
			return fmt.Errorf("read lease: %w", err)
		}
		if err := lease.Check(step.ClaimedBy, workerID, live); err != nil {
			return fmt.Errorf("step %s, worker %s: %w", stepID, workerID, err)
		}
		out, err = f(q, item, step, Actor{Kind: ActorWorker, ID: workerID})
		return err
	})
	return out, err
}

// ReportStepStage records progress on a step, resets its stuck timer, and
// records any outputs pushed so far.
func (s *Service) ReportStepStage(ctx context.Context, stepID, workerID, stage string, outputs []OutputInput) (db.WorkStep, error) {
	if err := validateOutputs(outputs); err != nil {
		return db.WorkStep{}, err
	}
	return s.asStepClaimant(ctx, stepID, workerID, func(q *db.Queries, item db.WorkItem, _ db.WorkStep, actor Actor) (db.WorkStep, error) {
		for _, o := range outputs {
			if _, err := recordOutput(ctx, q, item, o, actor); err != nil {
				return db.WorkStep{}, err
			}
		}
		step, err := q.SetStepStage(ctx, db.SetStepStageParams{ID: stepID, Stage: stage})
		if err != nil {
			return db.WorkStep{}, fmt.Errorf("set stage: %w", err)
		}
		_, err = writeEvent(ctx, q, item.ID, actor, EventStepStage, "", "", stepPayload(step, map[string]any{"stage": stage}))
		return step, err
	})
}

// ReleaseStep gives a step back: ready again, not counted as an attempt.
func (s *Service) ReleaseStep(ctx context.Context, stepID, workerID, reason string) (db.WorkStep, error) {
	return s.asStepClaimant(ctx, stepID, workerID, func(q *db.Queries, item db.WorkItem, _ db.WorkStep, actor Actor) (db.WorkStep, error) {
		step, err := q.ReturnStep(ctx, db.ReturnStepParams{ID: stepID})
		if err != nil {
			return db.WorkStep{}, fmt.Errorf("release step: %w", err)
		}
		_, err = writeEvent(ctx, q, item.ID, actor, EventStepReleased, "", "", stepPayload(step, map[string]any{"reason": reason}))
		return step, err
	})
}

// ReportStepResult records a worker's outcome for its step, then moves the
// graph and the item on.
func (s *Service) ReportStepResult(ctx context.Context, stepID, workerID string, r StepResult) (db.WorkStep, error) {
	if err := r.validate(); err != nil {
		return db.WorkStep{}, err
	}
	return s.asStepClaimant(ctx, stepID, workerID, func(q *db.Queries, item db.WorkItem, step db.WorkStep, actor Actor) (db.WorkStep, error) {
		return s.finishStep(ctx, q, item, step, r, actor, EventStepResult)
	})
}

// CompleteWait completes a wait step (await_checks, await_approval) that is
// waiting, as the server's poller or an adapter does when it sees the event.
// Its step_completed event carries the result. A step that is no longer
// waiting is ErrNotWaiting and nothing changes, so a second completer of the
// same event (a poll and a webhook) has no effect.
func (s *Service) CompleteWait(ctx context.Context, stepID string, r StepResult, actor Actor) (db.WorkStep, error) {
	if err := r.validate(); err != nil {
		return db.WorkStep{}, err
	}
	var out db.WorkStep
	err := s.inTx(ctx, func(q *db.Queries) error {
		item, step, current, err := lockStep(ctx, q, stepID)
		if err != nil {
			return err
		}
		if !plan.IsWait(step.Kind) || step.State != plan.Ready || !current || !active(item.State) {
			return fmt.Errorf("step %s (%s, %s): %w", step.Key, step.Kind, step.State, ErrNotWaiting)
		}
		out, err = s.finishStep(ctx, q, item, step, r, actor, EventStepCompleted)
		return err
	})
	return out, err
}

// finishStep records outputs, applies the outcome to the step, schedules
// what follows, and derives the item's state. The caller holds the locks.
func (s *Service) finishStep(ctx context.Context, q *db.Queries, item db.WorkItem, step db.WorkStep, r StepResult, actor Actor, eventType string) (db.WorkStep, error) {
	for _, o := range r.Outputs {
		if _, err := recordOutput(ctx, q, item, o, actor); err != nil {
			return db.WorkStep{}, err
		}
	}
	result := r.Result
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	extra := map[string]any{"outcome": r.Outcome, "reason": r.Reason, "retryable": r.Retryable, "outputs": len(r.Outputs)}
	if eventType == EventStepCompleted {
		// What the completer observed, such as the checks it counted.
		extra["result"] = result
	}
	if _, err := writeEvent(ctx, q, item.ID, actor, eventType, "", "", stepPayload(step, extra)); err != nil {
		return db.WorkStep{}, err
	}
	switch {
	case r.Outcome == StepBlocked:
		return endStepAndItem(ctx, q, item, step, result, Blocked, r.Reason, actor)
	case r.Outcome == StepFailed:
		return s.failStepAttempt(ctx, q, item, step, result, r.Reason, r.Retryable, actor)
	case plan.RoundsExhausted(step.Params, result):
		reason := fmt.Sprintf("%s: verdict %q on the last round", step.Key, plan.Verdict(result))
		return endStepAndItem(ctx, q, item, step, result, Failed, reason, actor)
	}
	step, err := q.FinishStep(ctx, db.FinishStepParams{ID: step.ID, State: plan.Succeeded, Result: result})
	if err != nil {
		return db.WorkStep{}, fmt.Errorf("finish step: %w", err)
	}
	if err := schedule(ctx, q, step.GraphID); err != nil {
		return db.WorkStep{}, err
	}
	return step, derive(ctx, q, item.ID, step.GraphID, actor)
}

// failStepAttempt counts an attempt on the step, then hands it out again
// after the retry delay, or gives up and fails the item.
func (s *Service) failStepAttempt(ctx context.Context, q *db.Queries, item db.WorkItem, step db.WorkStep, result json.RawMessage, reason string, retryable bool, actor Actor) (db.WorkStep, error) {
	step, err := q.IncrementStepAttempts(ctx, step.ID)
	if err != nil {
		return db.WorkStep{}, fmt.Errorf("count attempt: %w", err)
	}
	if s.policy.AfterFailure(int(step.Attempts), retryable) == lease.GiveUp {
		return endStepAndItem(ctx, q, item, step, result, Failed, fmt.Sprintf("step %s failed after %d attempts: %s", step.Key, step.Attempts, reason), actor)
	}
	step, err = q.ReturnStep(ctx, db.ReturnStepParams{ID: step.ID, DelaySeconds: s.policy.RetryDelay.Seconds()})
	if err != nil {
		return db.WorkStep{}, fmt.Errorf("retry step: %w", err)
	}
	return step, nil
}

// endStepAndItem marks the step failed and moves the item to failed or
// blocked, which cancels the graph's unfinished steps. An item that is no
// longer being worked keeps its state.
func endStepAndItem(ctx context.Context, q *db.Queries, item db.WorkItem, step db.WorkStep, result json.RawMessage, to, reason string, actor Actor) (db.WorkStep, error) {
	step, err := q.FinishStep(ctx, db.FinishStepParams{ID: step.ID, State: plan.Failed, Result: result})
	if err != nil {
		return db.WorkStep{}, fmt.Errorf("fail step: %w", err)
	}
	if !active(item.State) {
		return step, nil
	}
	_, err = transition(ctx, q, item, to, reason, actor, EventTransition, stepPayload(step, map[string]any{"attempts": step.Attempts}))
	return step, err
}

// derive applies the output rules to an item whose graph moved on. Once no
// open_pr step is left to run, the item has nothing more to produce (#4's
// outputs_done): in_review while an output is open, completed once every
// output has landed. A graph that finished without any output fails.
func derive(ctx context.Context, q *db.Queries, itemID, graphID string, actor Actor) error {
	item, err := q.GetItemForUpdate(ctx, itemID)
	if err != nil {
		return fmt.Errorf("lock item: %w", err)
	}
	if !active(item.State) {
		return nil
	}
	if item.OutputsDone {
		_, err := settle(ctx, q, item, actor)
		return err
	}
	steps, err := q.ListSteps(ctx, graphID)
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}
	done := func(st db.WorkStep) bool { return st.State == plan.Succeeded || st.State == plan.Skipped }
	allDone := !slices.ContainsFunc(steps, func(st db.WorkStep) bool { return !done(st) })
	producing := slices.ContainsFunc(steps, func(st db.WorkStep) bool { return st.Kind == plan.OpenPR && !done(st) })
	if producing && !allDone {
		return nil
	}
	_, err = finishOutputs(ctx, q, item, actor)
	switch {
	case errors.Is(err, ErrNoOutputs) && allDone:
		_, err = transition(ctx, q, item, Failed, "the graph finished without recording an output", actor, EventTransition, nil)
		return err
	case errors.Is(err, ErrNoOutputs):
		return nil
	}
	return err
}

// Graph returns the item's latest graph: the current one, or the last one
// superseded or handed over.
func (s *Service) Graph(ctx context.Context, itemID string) (GraphView, error) {
	if _, err := s.Get(ctx, itemID); err != nil {
		return GraphView{}, err
	}
	q := db.New(s.pool)
	graph, err := q.LatestGraph(ctx, itemID)
	if err != nil {
		return GraphView{}, notFound(err, "graph of work item "+itemID)
	}
	return graphView(ctx, q, graph)
}

func graphView(ctx context.Context, q *db.Queries, graph db.WorkGraph) (GraphView, error) {
	steps, err := q.ListSteps(ctx, graph.ID)
	if err != nil {
		return GraphView{}, fmt.Errorf("list steps: %w", err)
	}
	edges, err := q.ListEdges(ctx, graph.ID)
	if err != nil {
		return GraphView{}, fmt.Errorf("list edges: %w", err)
	}
	rows, err := q.ListStepOutputs(ctx, graph.ID)
	if err != nil {
		return GraphView{}, fmt.Errorf("list step outputs: %w", err)
	}
	outputs := make([]StepOutputInfo, len(rows))
	for i, r := range rows {
		outputs[i] = StepOutputInfo(r)
	}
	return GraphView{Graph: graph, Steps: steps, Edges: edges, StepOutputs: outputs}, nil
}

// Replan plans an item that is being worked again, after a template or
// config change: it reads the repositories' configs again, and a new graph
// version replaces the current one, whose unfinished steps are canceled.
// Workers holding them lose them. An unusable config is ErrInvalid and
// leaves the graph as it was; an unreadable one is ErrConfigUnavailable.
func (s *Service) Replan(ctx context.Context, itemID string, actor Actor) (GraphView, error) {
	var view GraphView
	err := s.inTx(ctx, func(q *db.Queries) error {
		item, err := q.GetItemForUpdate(ctx, itemID)
		if err != nil {
			return notFound(err, "work item "+itemID)
		}
		if !active(item.State) {
			return fmt.Errorf("work item %s is %s: %w", itemID, item.State, ErrNotReplannable)
		}
		configs, reads, err := s.readConfigs(ctx, item.Repos)
		if errors.Is(err, ErrBadRepoConfig) {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if err != nil {
			return err
		}
		g, err := plan.Plan(plan.Item{ID: item.ID, Title: item.Title, Repos: item.Repos}, configs, plan.DefaultTemplate())
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		graph, err := writeGraph(ctx, q, itemID, g, reads, actor)
		if err != nil {
			return err
		}
		view, err = graphView(ctx, q, graph)
		return err
	})
	return view, err
}

// PollPulls returns the open pull requests of the items being worked, for
// the server's poller, highest-priority item first. The newest pull request
// of each repository carries the ready wait step of its chain, if any.
func (s *Service) PollPulls(ctx context.Context) ([]db.PollPullsRow, error) {
	rows, err := db.New(s.pool).PollPulls(ctx, plan.WaitKinds())
	if err != nil {
		return nil, fmt.Errorf("list open pull requests: %w", err)
	}
	return rows, nil
}
