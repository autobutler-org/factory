// Times and states as people read them.

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

export const label = (state: string) => state.replaceAll("_", " ");

export function State({ state }: { state: string }) {
  return <span className={`state state-${state}`}>{label(state)}</span>;
}

export function Status({ error, updated }: { error?: Error; updated?: Date }) {
  return (
    <p className="status" role="status">
      {error ? <span className="error">Refresh failed: {error.message}. </span> : null}
      {updated ? `Updated ${updated.toLocaleTimeString()}.` : "Loading…"}
    </p>
  );
}
