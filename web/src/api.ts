// The API's JSON shapes (docs/api.md) and a fetch wrapper that sends the token.
// ponytail: hand-written until the OpenAPI document (#5) exists to generate them from.

export const STATES = [
  "design",
  "ready",
  "in_progress",
  "in_review",
  "blocked",
  "failed",
  "completed",
  "wont_fix",
] as const;

export type WorkItem = {
  id: string;
  source: string | null;
  external_id: string | null;
  external_url: string | null;
  title: string;
  body: string;
  repos: string[];
  state: string;
  stage: string;
  state_reason: string;
  priority: number;
  attempts: number;
  claimed_by: string | null;
  lease_expires_at: string | null;
  requires: string[];
  created_at: string;
  updated_at: string;
  state_changed_at: string;
};

export type WorkList = { items: WorkItem[]; next_cursor: string };

export type WorkEvent = {
  id: number;
  at: string;
  actor_kind: string;
  actor_id: string;
  type: string;
  from_state: string | null;
  to_state: string | null;
  payload: Record<string, unknown> | null;
};

export type Output = {
  id: number;
  repo: string;
  kind: string;
  branch: string;
  url: string;
  sha: string;
  status: string;
};

export type Step = {
  id: string;
  position: number;
  key: string;
  kind: string;
  sandbox_key: string;
  state: string;
  attempts: number;
  claimed_by: string | null;
  stage: string;
  result: unknown;
  started_at: string | null;
  finished_at: string | null;
};

// What is kept of a step's gate output (GET /steps/{id}/output), without the output.
export type StepOutput = { step_id: string; bytes: number; created_at: string };

export type Graph = {
  graph: { id: string; version: number; template: string; state: string; created_at: string };
  steps: Step[];
  edges: { step_id: string; depends_on_id: string }[];
  step_outputs: StepOutput[];
};

export type Hold = { work_item_id: string; title: string; step_id?: string; step_key?: string };

export type Worker = {
  id: string;
  name: string;
  kind: string;
  capabilities: { harnesses?: string[]; labels?: string[] };
  version: string;
  registered_at: string;
  last_seen_at: string;
  state: string;
  holds: Hold[];
};

// A worker sandbox (Kubernetes Job) the launcher ran for a work item.
export type Sandbox = {
  name: string;
  key: string;
  repo: string;
  image: string;
  phase: string;
  created: string;
};

export type LogLine = { time: string; text: string; truncated?: boolean };

// One read of a sandbox's log: GET /work/{id}/sandboxes/{name}/logs?tail= or ?after=.
export type LogRead = {
  sandbox: Sandbox;
  lines: LogLine[];
  cursor: string;
  more: boolean;
  pending: { reason: string; message: string } | null;
};

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export type Get = <T>(path: string, signal?: AbortSignal) => Promise<T>;

// getter returns a GET for /api/v1 paths that sends the token as a bearer token. A JSON reply is
// decoded; a plain text one (a step's gate output) is returned as a string.
export function getter(token: string): Get {
  return async <T>(path: string, signal?: AbortSignal) => {
    const resp = await fetch(`/api/v1${path}`, {
      headers: { Authorization: `Bearer ${token}` },
      signal,
    });
    if (!resp.ok) {
      const body = await resp.json().catch(() => null);
      throw new ApiError(
        resp.status,
        body?.error?.code ?? "",
        body?.error?.message ?? `${resp.status} ${resp.statusText}`
      );
    }
    if (resp.headers.get("Content-Type")?.startsWith("text/plain")) {
      return (await resp.text()) as T;
    }
    return (await resp.json()) as T;
  };
}
