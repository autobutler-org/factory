import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { LogLine, LogRead } from "./api";
import { getter } from "./api";
import { MAX_LINES, SandboxPage, Sandboxes, summarize } from "./Logs";
import { ApiContext, POLL_MS } from "./poll";
import { mockAPI, renderWithAPI, Reply } from "./testing";

const ID = "i1";
const NAME = "worker-3kq9x2m7a1bz";
const BASE = `/work/${ID}/sandboxes/${NAME}/logs`;
const TAIL = `${BASE}?tail=${MAX_LINES}`;
const after = (cursor: string) => `${BASE}?after=${cursor}`;

const sandbox = (phase = "running") => ({
  name: NAME,
  key: `${ID}/autobutler-org/quark`,
  repo: "autobutler-org/quark",
  image: "ghcr.io/autobutler-org/factory-worker:0.4.0",
  phase,
  created: "2026-11-20T10:00:00Z",
});

const line = (text: string): LogLine => ({ time: "2026-11-20T10:00:01Z", text });
const read = (lines: LogLine[], cursor: string, extra: Partial<LogRead> = {}): LogRead => ({
  sandbox: sandbox(),
  lines,
  cursor,
  more: false,
  pending: null,
  ...extra,
});

const log = () => screen.getByRole("region", { name: "Log of autobutler-org/quark" });

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => vi.useRealTimers());

test("the pane follows the log with the cursor, appending what is new", async () => {
  const routes: Record<string, unknown> = { [TAIL]: read([line("first")], "c1") };
  const fetch = mockAPI(routes);
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  expect(await screen.findByText("first")).toBeInTheDocument();
  expect(screen.getByRole("heading", { level: 1, name: "Worker log" })).toBeInTheDocument();
  expect(document.title).toBe(`Log of ${NAME} · factory`);
  expect(screen.getByText("running")).toBeInTheDocument();

  routes[after("c1")] = read([line("second")], "c2");
  await vi.advanceTimersByTimeAsync(POLL_MS);
  expect(await within(log()).findByText("second")).toBeInTheDocument();
  expect(within(log()).getByText("first")).toBeInTheDocument();
  expect(fetch.mock.calls.map(([u]) => String(u))).toEqual([
    `/api/v1${TAIL}`,
    `/api/v1${after("c1")}`,
  ]);
  // The log is not a live region: new lines are not announced.
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

test("more means read again at once", async () => {
  mockAPI({
    [TAIL]: read([line("part one")], "c1", { more: true }),
    [after("c1")]: read([line("part two")], "c2"),
  });
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  expect(await screen.findByText("part two")).toBeInTheDocument();
  expect(screen.getByText("part one")).toBeInTheDocument();
});

test("pausing the pane stops following until it resumes", async () => {
  const routes: Record<string, unknown> = { [TAIL]: read([line("first")], "c1") };
  const fetch = mockAPI(routes);
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  await screen.findByText("first");
  const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  const pause = screen.getByRole("button", { name: "Pause following" });
  await user.click(pause);
  expect(pause).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByText(/^Updates paused/)).toBeInTheDocument();

  routes[after("c1")] = read([line("second")], "c2");
  const calls = fetch.mock.calls.length;
  await vi.advanceTimersByTimeAsync(3 * POLL_MS);
  expect(fetch.mock.calls.length).toBe(calls);

  await user.click(pause);
  expect(await screen.findByText("second")).toBeInTheDocument();
});

test("the global Pause updates also pauses following", async () => {
  const fetch = mockAPI({ [TAIL]: read([line("first")], "c1") });
  render(
    <ApiContext.Provider value={{ get: getter("t"), refused: () => {}, paused: true }}>
      <SandboxPage id={ID} name={NAME} />
    </ApiContext.Provider>
  );
  await vi.advanceTimersByTimeAsync(2 * POLL_MS);
  expect(fetch).not.toHaveBeenCalled();
  expect(screen.getByText("Updates paused.")).toBeInTheDocument();
});

test("a container that has not started shows its reason, then the log with tail", async () => {
  const pending = { reason: "ContainerCreating", message: "pulling the image" };
  const routes: Record<string, unknown> = { [TAIL]: read([], "", { pending }) };
  mockAPI(routes);
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  expect(await screen.findByText(/Waiting for the worker container to start/)).toHaveTextContent(
    "ContainerCreating. pulling the image"
  );
  expect(screen.queryByText("No output yet.")).not.toBeInTheDocument();

  routes[TAIL] = read([line("started")], "c1");
  await vi.advanceTimersByTimeAsync(POLL_MS);
  expect(await screen.findByText("started")).toBeInTheDocument();
  expect(screen.queryByText(/Waiting for/)).not.toBeInTheDocument();
});

test("an ended sandbox says its log is complete and stops reading", async () => {
  const fetch = mockAPI({
    [TAIL]: read([line("done")], "c1", { sandbox: sandbox("succeeded") }),
  });
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  expect(
    await screen.findByText(/The sandbox succeeded, so its log is complete/)
  ).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pause following" })).not.toBeInTheDocument();
  await vi.advanceTimersByTimeAsync(3 * POLL_MS);
  expect(fetch).toHaveBeenCalledTimes(1);
});

test("a gone sandbox keeps what was read and says why it stopped", async () => {
  const routes: Record<string, unknown> = { [TAIL]: read([line("before")], "c1") };
  const fetch = mockAPI(routes);
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  await screen.findByText("before");
  // No route for after=c1: the mock answers 404.
  await vi.advanceTimersByTimeAsync(POLL_MS);
  expect(await screen.findByText(/no sandbox by that name/)).toHaveTextContent(
    "The lines below were read before it was removed."
  );
  expect(screen.getByText("before")).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: /error/i })).not.toBeInTheDocument();
  const calls = fetch.mock.calls.length;
  await vi.advanceTimersByTimeAsync(3 * POLL_MS);
  expect(fetch.mock.calls.length).toBe(calls);
});

test("a server without the launcher says logs are not available", async () => {
  const off = new Reply(503, {
    error: { code: "sandboxes_not_configured", message: "the server runs without --launcher" },
  });
  mockAPI({ [TAIL]: off, [`/work/${ID}/sandboxes`]: off });
  renderWithAPI(
    <>
      <SandboxPage id={ID} name={NAME} />
      <Sandboxes id={ID} />
    </>
  );
  await vi.waitFor(() =>
    expect(screen.getAllByText(/Worker logs are not available/)).toHaveLength(2)
  );
  expect(screen.queryByText(/Could not load/)).not.toBeInTheDocument();
});

test("an item lists a log pane per sandbox, or says it has none", async () => {
  const routes: Record<string, unknown> = {
    [`/work/${ID}/sandboxes`]: { sandboxes: [sandbox()] },
    [TAIL]: read([line("hello")], "c1"),
  };
  mockAPI(routes);
  renderWithAPI(<Sandboxes id={ID} />);
  const pane = await screen.findByRole("region", { name: `autobutler-org/quark ${NAME}` });
  expect(await within(pane).findByText("hello")).toBeInTheDocument();

  routes[`/work/${ID}/sandboxes`] = { sandboxes: [] };
  await vi.advanceTimersByTimeAsync(POLL_MS);
  expect(await screen.findByText(/No worker sandbox for this item/)).toBeInTheDocument();
});

test("the pane keeps only the last MAX_LINES lines", async () => {
  const many = Array.from({ length: MAX_LINES }, (_, i) => line(`line ${i}`));
  const routes: Record<string, unknown> = { [TAIL]: read(many, "c1") };
  mockAPI(routes);
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  await screen.findByText(`line ${MAX_LINES - 1}`);

  routes[after("c1")] = read([line("newest a"), line("newest b")], "c2");
  await vi.advanceTimersByTimeAsync(POLL_MS);
  await screen.findByText("newest b");
  expect(log().children).toHaveLength(MAX_LINES);
  expect(screen.queryByText("line 0")).not.toBeInTheDocument();
  expect(screen.queryByText("line 1")).not.toBeInTheDocument();
  expect(screen.getByText("line 2")).toBeInTheDocument();
  expect(
    screen.getByText(`Showing the last ${MAX_LINES.toLocaleString()} lines.`)
  ).toBeInTheDocument();
});

const slog = JSON.stringify({
  time: "2026-11-20T10:00:01Z",
  level: "WARN",
  msg: "claimed a step",
  step_id: "s1",
  attempt: 2,
});
const toolUse = JSON.stringify({
  type: "assistant",
  message: {
    content: [
      { type: "text", text: "Running the tests\nnow." },
      { type: "tool_use", name: "Bash", input: { command: "make check", description: "check" } },
    ],
  },
});

test("summarize reads slog records and agent events", () => {
  expect(summarize(slog)).toEqual({
    tag: "WARN",
    role: "warning",
    text: "claimed a step step_id=s1 attempt=2",
  });
  expect(summarize(toolUse)).toEqual({
    tag: "agent",
    text: "Running the tests now. · Bash: make check",
  });
  expect(
    summarize(
      JSON.stringify({
        type: "user",
        message: { content: [{ type: "tool_result", content: "x".repeat(1234) }] },
      })
    )
  ).toEqual({ tag: "tool", text: "result, 1,234 characters" });
  expect(
    summarize(JSON.stringify({ type: "system", subtype: "permission_denied", tool_name: "Write" }))
  ).toEqual({ tag: "system", role: "warning", text: "permission denied: Write" });
  expect(
    summarize(
      JSON.stringify({
        type: "result",
        subtype: "success",
        num_turns: 12,
        total_cost_usd: 0.4567,
        permission_denials: [{ tool_name: "Bash" }, { tool_name: "Bash" }],
      })
    )
  ).toEqual({
    tag: "result",
    role: "warning",
    text: "session finished, 12 turns, cost $0.46, 2 permission denials (Bash)",
  });
  expect(summarize("plain text {")).toEqual({ text: "plain text {" });
  expect(summarize('{"type":"unknown"}')).toEqual({ text: '{"type":"unknown"}' });
});

test("lines show as summaries, with the raw line a toggle away", async () => {
  mockAPI({
    [TAIL]: read([line(slog), line(toolUse), { ...line("x".repeat(20)), truncated: true }], "c1"),
  });
  renderWithAPI(<SandboxPage id={ID} name={NAME} />);
  await screen.findByRole("region", { name: "Log of autobutler-org/quark" });
  expect(within(log()).getByText(/claimed a step step_id=s1/)).toBeInTheDocument();
  expect(within(log()).getByText("WARN")).toBeInTheDocument();
  expect(within(log()).getByText(/Bash: make check/)).toBeInTheDocument();
  expect(within(log()).getAllByText(/cut at 16 KiB/)).toHaveLength(1);

  const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  await user.click(screen.getByRole("button", { name: "Show raw lines" }));
  expect(within(log()).getByText(slog)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Wrap lines" }));
  expect(log()).toHaveClass("nowrap");
});
