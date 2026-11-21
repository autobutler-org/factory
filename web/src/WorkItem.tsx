import { ApiError, type Get, type Graph, type Output, type WorkEvent, type WorkItem } from "./api";
import { label, State, Status, Time } from "./format";
import { usePoll } from "./poll";

async function load(id: string, get: Get, signal: AbortSignal) {
  const base = `/work/${encodeURIComponent(id)}`;
  const [item, events, outputs, graph] = await Promise.all([
    get<WorkItem>(base, signal),
    get<{ events: WorkEvent[] }>(`${base}/events`, signal),
    get<{ outputs: Output[] }>(`${base}/outputs`, signal),
    // 404 until the item is planned.
    get<Graph>(`${base}/graph`, signal).catch((err) => {
      if (err instanceof ApiError && err.status === 404) return null;
      throw err;
    }),
  ]);
  return { item, events: events.events, outputs: outputs.outputs, graph };
}

export function WorkItemPage({ id }: { id: string }) {
  const { data, error, updated } = usePoll(`work/${id}`, (get, signal) => load(id, get, signal));
  if (!data) {
    return (
      <>
        <h1>Work item</h1>
        <Status error={error} updated={updated} />
      </>
    );
  }
  const { item, events, outputs, graph } = data;
  return (
    <>
      <h1>{item.title}</h1>
      <Status error={error} updated={updated} />
      <dl className="facts">
        <dt>State</dt>
        <dd>
          <State state={item.state} />
        </dd>
        {item.state_reason ? (
          <>
            <dt>Reason</dt>
            <dd>
              <pre className="reason">{item.state_reason}</pre>
            </dd>
          </>
        ) : null}
        <dt>Stage</dt>
        <dd>{item.stage || "none"}</dd>
        <dt>Repositories</dt>
        <dd>{item.repos.join(", ") || "none"}</dd>
        <dt>Source</dt>
        <dd>
          {item.external_url ? (
            <a href={item.external_url} rel="noreferrer">
              {item.external_id ?? item.external_url}
            </a>
          ) : (
            (item.source ?? "none")
          )}
        </dd>
        <dt>Priority</dt>
        <dd>{item.priority}</dd>
        <dt>Attempts</dt>
        <dd>{item.attempts}</dd>
        <dt>Claimed by</dt>
        <dd>{item.claimed_by ?? "nobody"}</dd>
        <dt>Created</dt>
        <dd>
          <Time at={item.created_at} />
        </dd>
        <dt>ID</dt>
        <dd>
          <code>{item.id}</code>
        </dd>
      </dl>

      <section aria-labelledby="body">
        <h2 id="body">Body</h2>
        {item.body ? <pre className="body">{item.body}</pre> : <p>No body.</p>}
      </section>

      <section aria-labelledby="outputs">
        <h2 id="outputs">Outputs</h2>
        {outputs.length === 0 ? (
          <p>No pull requests or commits yet.</p>
        ) : (
          <ul>
            {outputs.map((o) => (
              <li key={o.id}>
                {o.repo}: {o.kind === "pull_request" ? "pull request" : o.kind}{" "}
                {o.url ? (
                  <a href={o.url} rel="noreferrer">
                    {o.url}
                  </a>
                ) : (
                  <code>{o.sha}</code>
                )}{" "}
                <State state={o.status} />
              </li>
            ))}
          </ul>
        )}
      </section>

      <Steps graph={graph} events={events} />
      <Timeline events={events} />
    </>
  );
}

// reasonOf is the step's latest reported reason: steps keep a result, not a reason, so it comes
// from the step's events.
function reasonOf(stepID: string, events: WorkEvent[]): string {
  for (let i = events.length - 1; i >= 0; i--) {
    const p = events[i].payload;
    if (p?.step_id === stepID && typeof p.reason === "string" && p.reason) return p.reason;
  }
  return "";
}

function Steps({ graph, events }: { graph: Graph | null; events: WorkEvent[] }) {
  if (!graph) {
    return (
      <section aria-labelledby="steps">
        <h2 id="steps">Steps</h2>
        <p>Not planned yet.</p>
      </section>
    );
  }
  const keys = new Map(graph.steps.map((s) => [s.id, s.key]));
  return (
    <section aria-labelledby="steps">
      <h2 id="steps">
        Steps <span className="count">graph v{graph.graph.version}</span>
      </h2>
      <ol className="steps">
        {graph.steps.map((s) => {
          const after = graph.edges
            .filter((e) => e.step_id === s.id)
            .map((e) => keys.get(e.depends_on_id));
          const reason = reasonOf(s.id, events);
          return (
            <li key={s.id} className={`step step-${s.state}`}>
              <h3>{s.key}</h3>
              <p className="meta">
                {s.kind} · <State state={s.state} /> · {s.attempts}{" "}
                {s.attempts === 1 ? "attempt" : "attempts"}
                {s.stage ? ` · ${s.stage}` : ""}
                {after.length ? ` · after ${after.join(", ")}` : ""}
              </p>
              {reason ? (
                <details open={s.state === "failed"}>
                  <summary>{s.state === "failed" ? "Failure reason" : "Last reason"}</summary>
                  <pre className="reason">{reason}</pre>
                </details>
              ) : null}
              {s.result != null ? (
                <details>
                  <summary>Result</summary>
                  <pre className="reason">{JSON.stringify(s.result, null, 2)}</pre>
                </details>
              ) : null}
            </li>
          );
        })}
      </ol>
    </section>
  );
}

// Reasons longer than this fold in the timeline.
const LONG = 300;

function Timeline({ events }: { events: WorkEvent[] }) {
  return (
    <section aria-labelledby="timeline">
      <h2 id="timeline">Timeline</h2>
      <ol className="timeline">
        {events.map((e) => {
          const { reason, ...rest } = e.payload ?? {};
          return (
            <li key={e.id}>
              <Time at={e.at} /> <strong>{label(e.type)}</strong>
              {e.from_state || e.to_state
                ? ` ${label(e.from_state ?? "")} → ${label(e.to_state ?? "")}`
                : ""}{" "}
              <span className="meta">
                by {e.actor_kind} {e.actor_id}
              </span>
              {typeof reason === "string" && reason.length > LONG ? (
                // The step graph shows a step's reason open; here it folds so the timeline reads.
                <details>
                  <summary>Reason ({reason.split("\n").length} lines)</summary>
                  <pre className="reason">{reason}</pre>
                </details>
              ) : typeof reason === "string" && reason ? (
                <pre className="reason">{reason}</pre>
              ) : null}
              {Object.keys(rest).length ? (
                <details>
                  <summary>Details</summary>
                  <pre className="reason">{JSON.stringify(rest, null, 2)}</pre>
                </details>
              ) : null}
            </li>
          );
        })}
      </ol>
    </section>
  );
}
