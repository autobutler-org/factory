import { useRef, useState, type FormEvent } from "react";
import { flushSync } from "react-dom";
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
  const repoField = useRef<HTMLInputElement>(null);
  // Clear filters removes itself, so focus moves to the first field instead of the page body.
  const clear = () => {
    flushSync(() => setFilter({ repo: "", source: "" }));
    repoField.current?.focus();
  };

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
      {/* The key resets the fields to the filter in force, so Clear filters empties them. */}
      <form
        key={`${filter.repo}\n${filter.source}`}
        className="filters"
        onSubmit={apply}
        aria-label="Filter work"
      >
        <label>
          Repository{" "}
          <input ref={repoField} name="repo" placeholder="owner/name" defaultValue={filter.repo} />
        </label>
        <label>
          Source <input name="source" placeholder="github" defaultValue={filter.source} />
        </label>
        <button type="submit">Filter</button>
      </form>
      {filter.repo || filter.source ? (
        <p className="filtered">
          Showing only work{filter.repo ? ` in ${filter.repo}` : ""}
          {filter.source ? ` from ${filter.source}` : ""}.{" "}
          <button type="button" onClick={clear}>
            Clear filters
          </button>
        </p>
      ) : null}
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
                {next_cursor ? <p className="meta">Showing the first {LIMIT}.</p> : null}
                {items.length === 0 ? (
                  <p className="empty">No work items.</p>
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
