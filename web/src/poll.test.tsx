import { render, screen } from "@testing-library/react";
import { getter } from "./api";
import { ApiContext, POLL_MS } from "./poll";
import { mockAPI, renderWithAPI, Reply } from "./testing";
import { Workers } from "./Workers";

// The states every polled page shares, shown through the workers page.
const workers = { workers: [] };
const down = new Reply(503, { error: { code: "unavailable", message: "database unavailable" } });

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => vi.useRealTimers());

test("while loading it says so, outside the live region", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(() => new Promise(() => {}));
  renderWithAPI(<Workers />);
  expect(screen.getByText("Loading…")).toBeInTheDocument();
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

test("a fresh page shows when it was updated without announcing it", async () => {
  mockAPI({ "/workers": workers });
  renderWithAPI(<Workers />);
  expect(await screen.findByText(/^Updated /)).toBeInTheDocument();
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

test("a failed first load says what failed and that it tries again", async () => {
  mockAPI({ "/workers": down });
  renderWithAPI(<Workers />);
  await vi.waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      "Could not load: database unavailable. Trying again every 5 seconds."
    )
  );
});

test("a failed refresh keeps the data and says it is stale", async () => {
  const routes: Record<string, unknown> = { "/workers": workers };
  mockAPI(routes);
  renderWithAPI(<Workers />);
  expect(await screen.findByText(/^No worker has registered\./)).toBeInTheDocument();

  routes["/workers"] = down;
  await vi.advanceTimersByTimeAsync(POLL_MS);
  await vi.waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      /^Could not refresh: database unavailable\. Showing data from .+\. Trying again every 5 seconds\.$/
    )
  );
  expect(screen.getByText(/^No worker has registered\./)).toBeInTheDocument();

  // It recovers by itself.
  routes["/workers"] = workers;
  await vi.advanceTimersByTimeAsync(POLL_MS);
  await vi.waitFor(() => expect(screen.getByRole("status")).toBeEmptyDOMElement());
});

test("paused updates stop polling and say so", async () => {
  const fetch = mockAPI({ "/workers": workers });
  const ui = (paused: boolean) => (
    <ApiContext.Provider value={{ get: getter("t0ken"), refused: () => {}, paused }}>
      <Workers />
    </ApiContext.Provider>
  );
  const { rerender } = render(ui(false));
  expect(await screen.findByText(/^Updated /)).toBeInTheDocument();

  rerender(ui(true));
  expect(screen.getByText(/^Updates paused\. Showing data from /)).toBeInTheDocument();
  const calls = fetch.mock.calls.length;
  await vi.advanceTimersByTimeAsync(3 * POLL_MS);
  expect(fetch.mock.calls.length).toBe(calls);

  // Resuming fetches at once.
  rerender(ui(false));
  await vi.waitFor(() => expect(fetch.mock.calls.length).toBe(calls + 1));
  expect(await screen.findByText(/^Updated /)).toBeInTheDocument();
});

test("something removed while shown keeps the last data and stops asking", async () => {
  const routes: Record<string, unknown> = { "/workers": workers };
  const fetch = mockAPI(routes);
  renderWithAPI(<Workers />);
  expect(await screen.findByText(/^No worker has registered\./)).toBeInTheDocument();

  delete routes["/workers"];
  await vi.advanceTimersByTimeAsync(POLL_MS);
  await vi.waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      /^Could not refresh: no \/workers\. Showing data from [^.]+\.$/
    )
  );
  const calls = fetch.mock.calls.length;
  await vi.advanceTimersByTimeAsync(3 * POLL_MS);
  expect(fetch.mock.calls.length).toBe(calls);
});
