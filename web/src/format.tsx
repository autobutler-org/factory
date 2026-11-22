import { useLayoutEffect, useRef, type ReactNode, type RefObject } from "react";
import { ApiError } from "./api";
import { POLL_MS } from "./poll";

// Times, states and page status as people read them.

export function Time({ at }: { at: string | null | undefined }) {
  if (!at) return <>never</>;
  const d = new Date(at);
  return <time dateTime={at}>{d.toLocaleString()}</time>;
}

export function ago(at: string, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(at).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

// label writes a state or event type as words. Headings capitalize it in CSS (.column h2).
export const label = (state: string) =>
  state === "wont_fix" ? "won't fix" : state.replaceAll("_", " ");

// The color role of every state the API returns: work items, steps, outputs and workers. One
// state, one role, everywhere; style.css colors each role from a token. Unknown states are neutral.
const ROLES: Record<string, "danger" | "warning" | "success" | "progress"> = {
  failed: "danger",
  gone: "danger",
  blocked: "warning",
  draining: "warning",
  completed: "success",
  succeeded: "success",
  merged: "success",
  active: "success",
  in_progress: "progress",
  in_review: "progress",
  running: "progress",
  open: "progress",
};

export const stateRole = (state: string) => ROLES[state] ?? "neutral";

// State writes the state out, so color is never its only cue.
export function State({ state }: { state: string }) {
  return <span className={`state state-${stateRole(state)}`}>{label(state)}</span>;
}

// Status says how fresh the page is. Only a refresh error is announced: the region is rendered
// empty first so screen readers pick up its text, while "Updated …", which changes every poll,
// is not announced (WCAG 4.1.3 without the chatter).
export function Status({
  error,
  updated,
  paused,
}: {
  error?: Error;
  updated?: Date;
  paused?: boolean;
}) {
  const at = updated?.toLocaleTimeString();
  const reason = error?.message.replace(/\.$/, "");
  const retry = paused
    ? "Resume updates to try again."
    : `Trying again every ${POLL_MS / 1000} seconds.`;
  // A 404 stops polling. Before any data the page shows its own not-found state; after, what
  // was shown is kept and there is nothing to retry.
  const missing = error instanceof ApiError && error.status === 404;
  let problem = "";
  if (error && missing && at) problem = `Could not refresh: ${reason}. Showing data from ${at}.`;
  else if (error && !missing) {
    problem = at
      ? `Could not refresh: ${reason}. Showing data from ${at}. ${retry}`
      : `Could not load: ${reason}. ${retry}`;
  }
  let fresh = "";
  if (!error) {
    if (!at) fresh = paused ? "Updates paused." : "Loading…";
    else fresh = paused ? `Updates paused. Showing data from ${at}.` : `Updated ${at}.`;
  }
  return (
    <div className="status">
      <p role="status" className="error">
        {problem}
      </p>
      {fresh ? <p>{fresh}</p> : null}
    </div>
  );
}

// Block is preformatted text that scrolls past 30rem. It carries a name, and joins the tab order
// while its text overflows, so a keyboard can scroll it (WCAG 2.1.1) without a tab stop on every
// short block. It measures after each render and when its box resizes (a window resize, or a
// <details> around it opening).
export function Block({
  label,
  className,
  children,
  ref,
  onScroll,
}: {
  label: string;
  className: string;
  children: ReactNode;
  ref?: RefObject<HTMLPreElement | null>;
  onScroll?: () => void;
}) {
  const own = useRef<HTMLPreElement>(null);
  const pre = ref ?? own;
  useLayoutEffect(() => {
    const el = pre.current;
    if (!el) return;
    // Set on the element, not through state, so measuring does not render again.
    const measure = () => {
      if (el.scrollHeight > el.clientHeight || el.scrollWidth > el.clientWidth) el.tabIndex = 0;
      else el.removeAttribute("tabindex");
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  });
  return (
    <pre ref={pre} className={className} role="region" aria-label={label} onScroll={onScroll}>
      {children}
    </pre>
  );
}
