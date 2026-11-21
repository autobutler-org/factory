import { useState, type FormEvent } from "react";
import { STATES, type WorkList } from "./api";
import { label, Status } from "./format";
import { Link, useTitle } from "./nav";
import { usePoll } from "./poll";

const LIMIT = 200;

// The board: one column per state, each its own query so a full column of finished work does
// not crowd out the live ones. A column with more than LIMIT items shows the first LIMIT.
export function Board() {
  useTitle("Board");
  const [filter, setFilter] = useState({ repo: "", source: "" });
  const query = new URLSearchParams({ limit: String(LIMIT) });
  if (filter.repo) query.set("repo", filter.repo);
  if (filter.source) query.set("source", filter.source);
  const poll = usePoll(query.toString(), (get, signal) =>
    Promise.all(STATES.map((s) => get<WorkList>(`/work?state=${s}&${query}`, signal)))
  );
  const { data } = poll;

  const apply = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    setFilter({
      repo: String(form.get("repo") ?? "").trim(),
      source: String(form.get("source") ?? "").trim(),
    });
  };

  return (
    <>
      <h1 tabIndex={-1}>Board</h1>
      <form className="filters" onSubmit={apply} aria-label="Filter work">
        <label>
          Repository <input name="repo" placeholder="owner/name" defaultValue={filter.repo} />
        </label>
        <label>
          Source <input name="source" placeholder="github" defaultValue={filter.source} />
        </label>
        <button type="submit">Filter</button>
      </form>
      <Status {...poll} />
      {data ? (
        <div className="board">
          {STATES.map((state, i) => {
            const { items, next_cursor } = data[i];
            const count = next_cursor ? `${items.length}+` : String(items.length);
            return (
              <section key={state} className="column" aria-labelledby={`col-${state}`}>
                <h2 id={`col-${state}`}>
                  {label(state)} <span className="count">{count}</span>
                </h2>
                {items.length === 0 ? (
                  <p className="empty">None</p>
                ) : (
                  <ul>
                    {items.map((item) => (
                      <li key={item.id} className="card">
                        <Link to={`/work/${item.id}`}>{item.title}</Link>
                        <p className="meta">
                          {item.repos.join(", ") || "no repository"}
                          {item.stage ? ` · ${item.stage}` : ""}
                          {item.priority ? ` · priority ${item.priority}` : ""}
                        </p>
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            );
          })}
        </div>
      ) : null}
    </>
  );
}
