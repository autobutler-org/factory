import type { Worker } from "./api";
import { ago, State, Status, Time } from "./format";
import { Link } from "./nav";
import { usePoll } from "./poll";

export function Workers() {
  const { data, error, updated } = usePoll("workers", (get, signal) =>
    get<{ workers: Worker[] }>("/workers", signal)
  );
  return (
    <>
      <h1>Workers</h1>
      <Status error={error} updated={updated} />
      {data && data.workers.length === 0 ? <p>No worker has registered.</p> : null}
      <ul className="cards">
        {data?.workers.map((w) => (
          <li key={w.id} className="card">
            <h2>{w.name}</h2>
            <dl>
              <dt>State</dt>
              <dd>
                <State state={w.state} />
              </dd>
              <dt>Kind</dt>
              <dd>{w.kind}</dd>
              <dt>Capabilities</dt>
              <dd>
                {[...(w.capabilities.harnesses ?? []), ...(w.capabilities.labels ?? [])].join(
                  ", "
                ) || "none"}
              </dd>
              <dt>Last seen</dt>
              <dd>
                {ago(w.last_seen_at)} (<Time at={w.last_seen_at} />)
              </dd>
              <dt>Version</dt>
              <dd>{w.version || "unknown"}</dd>
              <dt>Holds</dt>
              <dd>
                {w.holds.length === 0 ? (
                  "nothing"
                ) : (
                  <ul>
                    {w.holds.map((h) => (
                      <li key={h.step_id ?? h.work_item_id}>
                        <Link to={`/work/${h.work_item_id}`}>{h.title}</Link>
                        {h.step_key ? ` (step ${h.step_key})` : " (whole item)"}
                      </li>
                    ))}
                  </ul>
                )}
              </dd>
            </dl>
          </li>
        ))}
      </ul>
    </>
  );
}
