// A failed work item as the API returns it, with a gate failure several KB long, for tests.

const gateOutput = Array.from(
  { length: 120 },
  (_, i) =>
    `--- FAIL: TestCase${i} (0.0${i % 10}s)\n    case_test.go:${i + 10}: got ${i}, want ${i + 1}`
).join("\n");

export const ITEM_ID = "3f0c8a52-6a0e-4f62-9d55-2b9d7c1e4a10";
const steps = ["workspace", "implement", "gate", "open_pr"].map((key, position) => ({
  id: `step-${key}`,
  position,
  key: `autobutler-org/factory/${key}`,
  kind: key,
  sandbox_key: `${ITEM_ID}/autobutler-org/factory`,
  state: ["succeeded", "succeeded", "failed", "canceled"][position],
  attempts: [1, 1, 3, 0][position],
  claimed_by: null,
  stage: "",
  result: position === 0 ? { branch: "3f0c8a52_add-the-web-ui", base: "main" } : null,
  started_at: "2026-11-20T10:00:00Z",
  finished_at: "2026-11-20T10:05:00Z",
}));

export const failedItem = {
  item: {
    id: ITEM_ID,
    source: "github",
    external_id: "autobutler-org/factory#14",
    external_url: "https://github.com/autobutler-org/factory/issues/14",
    title: "build the factory web UI",
    body: "The factory needs a UI.\n\nIt is served by the same binary as the API.",
    repos: ["autobutler-org/factory"],
    state: "failed",
    stage: "gate",
    state_reason: "step autobutler-org/factory/gate failed after 3 attempts: make check failed",
    priority: 1,
    attempts: 0,
    claimed_by: null,
    lease_expires_at: null,
    requires: [],
    created_at: "2026-11-20T09:59:00Z",
    updated_at: "2026-11-20T10:05:00Z",
    state_changed_at: "2026-11-20T10:05:00Z",
  },
  events: {
    events: [
      {
        id: 1,
        at: "2026-11-20T09:59:00Z",
        actor_kind: "adapter",
        actor_id: "github",
        type: "created",
        from_state: null,
        to_state: "ready",
        payload: {},
      },
      {
        id: 2,
        at: "2026-11-20T10:00:00Z",
        actor_kind: "worker",
        actor_id: "worker-abc",
        type: "transition",
        from_state: "ready",
        to_state: "in_progress",
        payload: {},
      },
      {
        id: 3,
        at: "2026-11-20T10:00:01Z",
        actor_kind: "system",
        actor_id: "planner",
        type: "planned",
        from_state: null,
        to_state: null,
        payload: { version: 1 },
      },
      {
        id: 4,
        at: "2026-11-20T10:04:00Z",
        actor_kind: "worker",
        actor_id: "worker-abc",
        type: "step_result_reported",
        from_state: null,
        to_state: null,
        payload: {
          step_id: "step-gate",
          key: "autobutler-org/factory/gate",
          kind: "gate",
          outcome: "failed",
          reason: gateOutput,
          retryable: false,
        },
      },
      {
        id: 5,
        at: "2026-11-20T10:05:00Z",
        actor_kind: "worker",
        actor_id: "worker-abc",
        type: "transition",
        from_state: "in_progress",
        to_state: "failed",
        payload: {
          step_id: "step-gate",
          key: "autobutler-org/factory/gate",
          kind: "gate",
          attempts: 3,
          reason: "step autobutler-org/factory/gate failed after 3 attempts: " + gateOutput,
        },
      },
    ],
  },
  outputs: {
    outputs: [
      {
        id: 1,
        repo: "autobutler-org/factory",
        kind: "pull_request",
        branch: "3f0c8a52_add-the-web-ui",
        url: "https://github.com/autobutler-org/factory/pull/50",
        sha: "",
        status: "open",
      },
    ],
  },
  graph: {
    graph: {
      id: "g1",
      version: 1,
      template: "default",
      state: "current",
      created_at: "2026-11-20T10:00:01Z",
    },
    steps,
    edges: steps.slice(1).map((s, i) => ({ step_id: s.id, depends_on_id: steps[i].id })),
  },
  gateOutput,
};

export function itemRoutes(f = failedItem) {
  const base = `/work/${f.item.id}`;
  return {
    [base]: f.item,
    [`${base}/events`]: f.events,
    [`${base}/outputs`]: f.outputs,
    [`${base}/graph`]: f.graph,
    [`${base}/sandboxes`]: { sandboxes: [] },
  };
}
