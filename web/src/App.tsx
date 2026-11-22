import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { getter } from "./api";
import { Board } from "./Board";
import { SandboxPage } from "./Logs";
import { Link, usePath, useTitle } from "./nav";
import { ApiContext } from "./poll";
import { StepOutputPage } from "./StepOutput";
import { WorkItemPage } from "./WorkItem";
import { Workers } from "./Workers";

// The token lives in sessionStorage: it survives a reload, not the tab. Real sign-in is #15.
const TOKEN_KEY = "factory.apiToken";

function readToken(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

function writeToken(token: string) {
  try {
    if (token) sessionStorage.setItem(TOKEN_KEY, token);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    // Storage blocked: the token lasts until reload.
  }
}

export function App() {
  const [token, setToken] = useState(readToken);
  const [refusedOnce, setRefusedOnce] = useState(false);
  const [paused, setPaused] = useState(false);

  const save = (t: string) => {
    writeToken(t);
    setToken(t);
  };
  const refused = useCallback(() => {
    writeToken("");
    setToken("");
    setRefusedOnce(true);
  }, []);
  const api = useMemo(() => ({ get: getter(token), refused, paused }), [token, refused, paused]);

  return (
    <>
      <header>
        <nav aria-label="Main">
          <span className="brand">factory</span>
          <Link to="/">Board</Link>
          <Link to="/workers">Workers</Link>
          {token ? (
            <span className="actions">
              <button type="button" aria-pressed={paused} onClick={() => setPaused(!paused)}>
                Pause updates
              </button>
              <button type="button" className="link" onClick={() => save("")}>
                Forget token
              </button>
            </span>
          ) : null}
        </nav>
      </header>
      <main>
        {token ? (
          <ApiContext.Provider value={api}>
            <Page />
          </ApiContext.Provider>
        ) : (
          <TokenForm refused={refusedOnce} onSave={save} />
        )}
      </main>
    </>
  );
}

function Page() {
  const path = usePath();
  // After a client-side navigation, focus the new page's heading so keyboard and screen-reader
  // users start there, as they would after a full load. The first render keeps the browser's focus.
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    document.querySelector<HTMLElement>("main h1")?.focus();
  }, [path]);
  const log = /^\/work\/([^/]+)\/sandboxes\/([^/]+)\/?$/.exec(path);
  if (log) {
    const [id, name] = [decodeURIComponent(log[1]), decodeURIComponent(log[2])];
    return <SandboxPage key={path} id={id} name={name} />;
  }
  const output = /^\/work\/([^/]+)\/steps\/([^/]+)\/output\/?$/.exec(path);
  if (output) {
    const [id, step] = [decodeURIComponent(output[1]), decodeURIComponent(output[2])];
    return <StepOutputPage key={path} id={id} stepID={step} />;
  }
  const work = /^\/work\/([^/]+)\/?$/.exec(path);
  if (work) return <WorkItemPage key={work[1]} id={decodeURIComponent(work[1])} />;
  if (path === "/workers" || path === "/workers/") return <Workers />;
  if (path === "/") return <Board />;
  return <NotFound path={path} />;
}

function NotFound({ path }: { path: string }) {
  useTitle("Not found");
  return (
    <>
      <h1 tabIndex={-1}>Not found</h1>
      <p>
        No page at <code>{path}</code>. <Link to="/">Go to the board</Link>.
      </p>
    </>
  );
}

function TokenForm({ refused, onSave }: { refused: boolean; onSave: (token: string) => void }) {
  useTitle("API token");
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const t = String(new FormData(e.currentTarget).get("token") ?? "").trim();
    if (t) onSave(t);
  };
  return (
    <form className="token" onSubmit={submit}>
      <h1 tabIndex={-1}>API token</h1>
      {refused ? (
        <p id="token-help" role="alert" className="error">
          The server refused that token. Enter the current FACTORY_API_TOKEN.
        </p>
      ) : (
        <p id="token-help">
          Enter the server&apos;s FACTORY_API_TOKEN. It is kept for this tab only.
        </p>
      )}
      <label>
        Token{" "}
        <input
          name="token"
          type="password"
          autoComplete="off"
          required
          autoFocus
          spellCheck={false}
          aria-describedby="token-help"
          aria-invalid={refused || undefined}
        />
      </label>
      <button type="submit">Continue</button>
    </form>
  );
}
