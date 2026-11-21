import { useCallback, useMemo, useState, type FormEvent } from "react";
import { getter } from "./api";
import { Board } from "./Board";
import { Link, usePath } from "./nav";
import { ApiContext } from "./poll";
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

  const save = (t: string) => {
    writeToken(t);
    setToken(t);
  };
  const refused = useCallback(() => {
    writeToken("");
    setToken("");
    setRefusedOnce(true);
  }, []);
  const api = useMemo(() => ({ get: getter(token), refused }), [token, refused]);

  return (
    <>
      <header>
        <nav aria-label="Main">
          <span className="brand">factory</span>
          <Link to="/">Board</Link>
          <Link to="/workers">Workers</Link>
          {token ? (
            <button type="button" className="link" onClick={() => save("")}>
              Forget token
            </button>
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
  const work = /^\/work\/([^/]+)\/?$/.exec(path);
  if (work) return <WorkItemPage key={work[1]} id={decodeURIComponent(work[1])} />;
  if (path === "/workers" || path === "/workers/") return <Workers />;
  if (path === "/") return <Board />;
  return (
    <>
      <h1>Not found</h1>
      <p>
        No page at <code>{path}</code>. <Link to="/">Go to the board</Link>.
      </p>
    </>
  );
}

function TokenForm({ refused, onSave }: { refused: boolean; onSave: (token: string) => void }) {
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const t = String(new FormData(e.currentTarget).get("token") ?? "").trim();
    if (t) onSave(t);
  };
  return (
    <form className="token" onSubmit={submit}>
      <h1>API token</h1>
      {refused ? (
        <p role="alert" className="error">
          The server refused that token. Enter the current FACTORY_API_TOKEN.
        </p>
      ) : (
        <p>Enter the server&apos;s FACTORY_API_TOKEN. It is kept for this tab only.</p>
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
        />
      </label>
      <button type="submit">Continue</button>
    </form>
  );
}
