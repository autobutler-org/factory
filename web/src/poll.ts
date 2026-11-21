import { createContext, useContext, useEffect, useState } from "react";
import { ApiError, type Get } from "./api";

export const POLL_MS = 5000;

// The signed-in API: a GET that carries the token, what to do when the server refuses it, and
// whether the person paused updates (WCAG 2.2.2: auto-updating content can be paused).
export const ApiContext = createContext<{
  get: Get;
  refused: () => void;
  paused?: boolean;
} | null>(null);

export type Poll<T> = { data?: T; error?: ApiError | Error; updated?: Date; paused?: boolean };

// usePoll runs load now and every POLL_MS until unmounted, paused or key changes. A failed poll
// keeps the last data and reports the error; a 401 hands back to the token prompt; a 404 stops,
// since what it asked for does not exist. Resuming polls at once and keeps the data meanwhile.
export function usePoll<T>(
  key: string,
  load: (get: Get, signal: AbortSignal) => Promise<T>
): Poll<T> {
  const api = useContext(ApiContext);
  if (!api) throw new Error("usePoll needs an ApiContext");
  // The key is stored with the result, so a new key starts empty without resetting in the effect.
  const [poll, setPoll] = useState<Poll<T> & { key?: string }>({});
  const { get, refused, paused = false } = api;
  useEffect(() => {
    if (paused) return;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const run = async () => {
      try {
        const data = await load(get, abort.signal);
        setPoll({ key, data, updated: new Date() });
      } catch (err) {
        if (abort.signal.aborted) return;
        if (err instanceof ApiError && err.status === 401) {
          refused();
          return;
        }
        const error = err instanceof Error ? err : new Error(String(err));
        setPoll((p) => ({ ...(p.key === key ? p : {}), key, error }));
        if (err instanceof ApiError && err.status === 404) return;
      }
      if (!abort.signal.aborted) timer = setTimeout(run, POLL_MS);
    };
    void run();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
    // load is a new closure every render; key names what it fetches.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, get, refused, paused]);
  return poll.key === key ? { ...poll, paused } : { paused };
}
