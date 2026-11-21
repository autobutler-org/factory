import { createContext, useContext, useEffect, useState } from "react";
import { ApiError, type Get } from "./api";

export const POLL_MS = 5000;

// The signed-in API: a GET that carries the token, and what to do when the server refuses it.
export const ApiContext = createContext<{ get: Get; refused: () => void } | null>(null);

type Poll<T> = { data?: T; error?: ApiError | Error; updated?: Date };

// usePoll runs load now and every POLL_MS until unmounted or key changes. A failed poll keeps the
// last data and reports the error; a 401 hands back to the token prompt.
export function usePoll<T>(
  key: string,
  load: (get: Get, signal: AbortSignal) => Promise<T>
): Poll<T> {
  const api = useContext(ApiContext);
  if (!api) throw new Error("usePoll needs an ApiContext");
  const [poll, setPoll] = useState<Poll<T>>({});
  const { get, refused } = api;
  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const run = async () => {
      try {
        const data = await load(get, abort.signal);
        setPoll({ data, updated: new Date() });
      } catch (err) {
        if (abort.signal.aborted) return;
        if (err instanceof ApiError && err.status === 401) {
          refused();
          return;
        }
        setPoll((p) => ({ ...p, error: err instanceof Error ? err : new Error(String(err)) }));
      }
      if (!abort.signal.aborted) timer = setTimeout(run, POLL_MS);
    };
    setPoll({});
    void run();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
    // load is a new closure every render; key names what it fetches.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, get, refused]);
  return poll;
}
