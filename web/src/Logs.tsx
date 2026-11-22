import { useContext, useLayoutEffect, useRef, useState } from "react";
import { ApiError, type Get, type LogLine, type LogRead, type Sandbox } from "./api";
import { Block, label, State, Status } from "./format";
import { Link, useTitle } from "./nav";
import { ApiContext, usePoll } from "./poll";

// Worker logs: an item's sandboxes, and a pane per sandbox that follows its log by polling with
// the API's cursor (docs/api.md#sandbox-logs).

// The pane keeps the last MAX_LINES lines, in memory and in the DOM. Lines are cut at 16 KiB by
// the server, so this bounds a pane at about 16 MB of text in the worst case.
export const MAX_LINES = 1000;
// How many reads one poll makes while the server says there is more, before waiting for the next.
const MAX_READS = 10;

type Log = {
  sandbox?: Sandbox;
  lines: LogLine[];
  // How many lines fell off the front, so a line keeps its key as the window moves.
  dropped: number;
  cursor: string;
  pending: LogRead["pending"];
  gone?: boolean;
  unconfigured?: boolean;
};

const EMPTY: Log = { lines: [], dropped: 0, cursor: "", pending: null };

const ended = (s?: Sandbox) => s?.phase === "succeeded" || s?.phase === "failed";
const done = (log: Log) => log.gone || log.unconfigured || ended(log.sandbox);

// readLog reads what follows log: the last MAX_LINES lines when there is no cursor, the lines after
// it otherwise, again at once while the server says there is more. A sandbox that has ended, is
// gone, or that this server cannot read is not read again.
export async function readLog(log: Log, path: string, get: Get, signal: AbortSignal) {
  if (done(log)) return log;
  let next = log;
  for (let i = 0; i < MAX_READS; i++) {
    const query = next.cursor ? `after=${encodeURIComponent(next.cursor)}` : `tail=${MAX_LINES}`;
    let read: LogRead;
    try {
      read = await get<LogRead>(`${path}/logs?${query}`, signal);
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return { ...next, gone: true };
      if (err instanceof ApiError && err.code === "sandboxes_not_configured") {
        return { ...next, unconfigured: true };
      }
      throw err;
    }
    // Without a cursor the read is a tail, which replaces what was shown.
    const all = next.cursor ? [...next.lines, ...read.lines] : read.lines;
    const cut = Math.max(0, all.length - MAX_LINES);
    next = {
      sandbox: read.sandbox,
      lines: all.slice(cut),
      dropped: (next.cursor ? next.dropped : 0) + cut,
      cursor: read.cursor,
      pending: read.pending,
    };
    if (!read.more) break;
  }
  return next;
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown) => (typeof v === "string" ? v : "");

// one writes text on one line, cut at max characters.
function one(text: string, max = 300) {
  const t = text.replace(/\s+/g, " ").trim();
  return t.length > max ? `${t.slice(0, max)}…` : t;
}

function contentOf(event: Obj): Obj[] {
  const m = event.message;
  return isObj(m) && Array.isArray(m.content) ? m.content.filter(isObj) : [];
}

// toolCall is a tool's name and the input a reader needs: the command, file or pattern.
function toolCall(use: Obj) {
  const input = isObj(use.input) ? use.input : {};
  const what =
    str(input.command) ||
    str(input.file_path) ||
    str(input.path) ||
    str(input.pattern) ||
    str(input.url) ||
    str(input.description) ||
    JSON.stringify(input);
  return `${str(use.name) || "tool"}: ${one(what, 200)}`;
}

function sizeOf(content: unknown): number {
  if (typeof content === "string") return content.length;
  if (Array.isArray(content))
    return content.reduce((n, c) => n + (isObj(c) ? str(c.text).length : 0), 0);
  return content == null ? 0 : JSON.stringify(content).length;
}

const plural = (n: number, word: string) => `${n.toLocaleString()} ${word}${n === 1 ? "" : "s"}`;

export type Summary = { tag?: string; role?: "danger" | "warning"; text: string };

// summarize reads a worker line: a log/slog record as level, message and fields; an agent CLI
// stream-json event as one readable line; anything else as it is.
export function summarize(text: string): Summary {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return { text };
  }
  if (!isObj(v)) return { text };

  if (typeof v.level === "string" && typeof v.msg === "string") {
    const fields = Object.entries(v)
      .filter(([k]) => k !== "time" && k !== "level" && k !== "msg")
      .map(([k, x]) => `${k}=${typeof x === "string" ? x : JSON.stringify(x)}`);
    const level = v.level;
    const role = level.startsWith("ERROR")
      ? "danger"
      : level.startsWith("WARN")
        ? "warning"
        : undefined;
    return { tag: level, role, text: one([v.msg, ...fields].join(" "), 1000) };
  }

  switch (v.type) {
    case "assistant": {
      const parts = contentOf(v).map((c) =>
        c.type === "text"
          ? one(str(c.text))
          : c.type === "tool_use"
            ? toolCall(c)
            : label(str(c.type))
      );
      return { tag: "agent", text: parts.join(" · ") || "agent message" };
    }
    case "user": {
      const results = contentOf(v).filter((c) => c.type === "tool_result");
      if (!results.length) break;
      const failed = results.some((r) => r.is_error === true);
      const parts = results.map(
        (r) =>
          `${r.is_error === true ? "error" : "result"}, ${plural(sizeOf(r.content), "character")}`
      );
      return { tag: "tool", role: failed ? "warning" : undefined, text: parts.join(" · ") };
    }
    case "system": {
      const sub = str(v.subtype);
      if (sub.includes("permission")) {
        const tool = str(v.tool_name) || str(v.tool) || str(v.name);
        return {
          tag: "system",
          role: "warning",
          text: `permission denied${tool ? `: ${tool}` : ""}`,
        };
      }
      if (sub === "init") {
        return {
          tag: "system",
          text: `session started${v.model ? `, model ${str(v.model)}` : ""}`,
        };
      }
      return { tag: "system", text: one(`${label(sub || "event")} ${str(v.message)}`) };
    }
    case "result": {
      const denials = Array.isArray(v.permission_denials) ? v.permission_denials.filter(isObj) : [];
      const parts = [
        v.is_error === true ? "session failed" : "session finished",
        typeof v.num_turns === "number" ? plural(v.num_turns, "turn") : "",
        typeof v.total_cost_usd === "number" ? `cost $${v.total_cost_usd.toFixed(2)}` : "",
        denials.length
          ? `${plural(denials.length, "permission denial")} (${[...new Set(denials.map((d) => str(d.tool_name)))].join(", ")})`
          : "",
      ];
      const role = v.is_error === true ? "danger" : denials.length ? "warning" : undefined;
      return { tag: "result", role, text: parts.filter(Boolean).join(", ") };
    }
  }
  return { text };
}

// SandboxLog is one sandbox's log pane. It follows while the sandbox runs, unless the reader or the
// global Pause updates pauses it, and stops once the sandbox ends or is gone.
export function SandboxLog({ id, name, level }: { id: string; name: string; level: 2 | 3 }) {
  const api = useContext(ApiContext);
  const [paused, setPaused] = useState(false);
  if (!api) throw new Error("SandboxLog needs an ApiContext");
  // usePoll pauses on the context, so the pane's own pause is the global one or this one.
  return (
    <ApiContext.Provider value={{ ...api, paused: api.paused || paused }}>
      <LogBody id={id} name={name} level={level} paused={paused} setPaused={setPaused} />
    </ApiContext.Provider>
  );
}

function LogBody({
  id,
  name,
  level,
  paused,
  setPaused,
}: {
  id: string;
  name: string;
  level: 2 | 3;
  paused: boolean;
  setPaused: (paused: boolean) => void;
}) {
  const [wrap, setWrap] = useState(true);
  const [raw, setRaw] = useState(false);
  const path = `/work/${encodeURIComponent(id)}/sandboxes/${encodeURIComponent(name)}`;
  // The last read, for the next one to follow on from. usePoll keeps it for rendering.
  const last = useRef<Log>(EMPTY);
  const poll = usePoll(`logs/${path}`, async (get, signal) => {
    last.current = await readLog(last.current, path, get, signal);
    return last.current;
  });
  const log = poll.data ?? EMPTY;

  // Follow the end of the log while the reader is at it; once they scroll up, leave them there.
  // Lines dropped off the front are handled by the browser's scroll anchoring.
  const pre = useRef<HTMLPreElement>(null);
  const atEnd = useRef(true);
  const onScroll = () => {
    const el = pre.current;
    if (el) atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  };
  useLayoutEffect(() => {
    const el = pre.current;
    if (el && atEnd.current) el.scrollTop = el.scrollHeight;
  }, [log.lines, raw, wrap]);

  const H = level === 2 ? "h2" : "h3";
  const s = log.sandbox;
  return (
    <section className="log-pane" aria-labelledby={`log-${name}`}>
      <H id={`log-${name}`}>
        {s ? (
          <>
            {s.repo} <span className="count">{name}</span>
          </>
        ) : (
          name
        )}
      </H>
      {s ? (
        <p className="meta">
          <State state={s.phase} /> · {s.image}
        </p>
      ) : null}
      {log.unconfigured ? (
        <p>
          Worker logs are not available: this server runs without the launcher (
          <code>factory serve --launcher</code>).
        </p>
      ) : log.gone ? (
        <p>
          This item has no sandbox by that name: it ended and was removed, and its log with it, or
          the worker does not run in a sandbox.
          {log.lines.length ? " The lines below were read before it was removed." : ""}
        </p>
      ) : s && ended(s) ? (
        <p>
          The sandbox {label(s.phase)}, so its log is complete. Kubernetes removes it an hour after
          it ends.
        </p>
      ) : (
        <Status {...poll} />
      )}
      {log.pending ? (
        <p>
          Waiting for the worker container to start: <strong>{log.pending.reason}</strong>
          {log.pending.message ? `. ${log.pending.message}` : ""}
        </p>
      ) : null}
      {log.lines.length ? (
        <div className="toolbar">
          {done(log) ? null : (
            <button type="button" aria-pressed={paused} onClick={() => setPaused(!paused)}>
              Pause following
            </button>
          )}
          <button type="button" aria-pressed={wrap} onClick={() => setWrap(!wrap)}>
            Wrap lines
          </button>
          <button type="button" aria-pressed={raw} onClick={() => setRaw(!raw)}>
            Show raw lines
          </button>
        </div>
      ) : null}
      {log.lines.length === MAX_LINES ? (
        <p className="meta">Showing the last {MAX_LINES.toLocaleString()} lines.</p>
      ) : null}
      {log.lines.length ? (
        <Block
          ref={pre}
          onScroll={onScroll}
          className={wrap ? "log" : "log nowrap"}
          label={`Log of ${s?.repo ?? name}`}
        >
          {log.lines.map((line, i) => (
            <Line key={log.dropped + i} line={line} raw={raw} />
          ))}
        </Block>
      ) : poll.data && !log.pending && !log.gone && !log.unconfigured ? (
        <p>No output yet.</p>
      ) : null}
    </section>
  );
}

function Line({ line, raw }: { line: LogLine; raw: boolean }) {
  const cut = line.truncated ? <span className="tag"> [cut at 16 KiB]</span> : null;
  const time = (
    <time dateTime={line.time}>{line.time ? new Date(line.time).toLocaleTimeString() : ""}</time>
  );
  if (raw) {
    return (
      <span>
        {line.text}
        {cut}
        {"\n"}
      </span>
    );
  }
  const { tag, role, text } = summarize(line.text);
  return (
    <span>
      {time} {tag ? <span className={role ? `tag tag-${role}` : "tag"}>{tag} </span> : null}
      {text}
      {cut}
      {"\n"}
    </span>
  );
}

// Sandboxes lists the item's sandboxes, each with its log pane.
export function Sandboxes({ id }: { id: string }) {
  const poll = usePoll(`sandboxes/${id}`, (get, signal) =>
    get<{ sandboxes: Sandbox[] }>(`/work/${encodeURIComponent(id)}/sandboxes`, signal)
  );
  const { data, error } = poll;
  const unconfigured = error instanceof ApiError && error.code === "sandboxes_not_configured";
  return (
    <section aria-labelledby="logs">
      <h2 id="logs">Worker logs</h2>
      {unconfigured ? (
        <p>
          Worker logs are not available: this server runs without the launcher (
          <code>factory serve --launcher</code>).
        </p>
      ) : !data || error ? (
        <Status {...poll} />
      ) : null}
      {data && data.sandboxes.length === 0 ? (
        <p>
          No worker sandbox for this item. The launcher starts one per repository while the item has
          steps to run; an ended one is removed an hour later, with its log.
        </p>
      ) : null}
      {data?.sandboxes.map((s) => (
        <SandboxLog key={s.name} id={id} name={s.name} level={3} />
      ))}
    </section>
  );
}

// SandboxPage is one sandbox's log on its own, the page the Workers page links to.
export function SandboxPage({ id, name }: { id: string; name: string }) {
  useTitle(`Log of ${name}`);
  return (
    <>
      <h1 tabIndex={-1}>Worker log</h1>
      <p>
        <Link to={`/work/${encodeURIComponent(id)}`}>Go to the work item</Link>
      </p>
      <SandboxLog id={id} name={name} level={2} />
    </>
  );
}
