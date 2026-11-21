import { screen, within } from "@testing-library/react";
import { failedItem, itemRoutes, ITEM_ID } from "./fixtures";
import { POLL_MS } from "./poll";
import { mockAPI, renderWithAPI } from "./testing";
import { WorkItemPage } from "./WorkItem";

test("the work item shows its facts, outputs and body", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);

  expect(
    await screen.findByRole("heading", { level: 1, name: "build the factory web UI" })
  ).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "autobutler-org/factory#14" })).toHaveAttribute(
    "href",
    "https://github.com/autobutler-org/factory/issues/14"
  );
  const outputs = screen.getByRole("region", { name: "Outputs" });
  expect(
    within(outputs).getByRole("link", { name: "https://github.com/autobutler-org/factory/pull/50" })
  ).toBeInTheDocument();
  expect(within(outputs).getByText("open")).toBeInTheDocument();
  expect(screen.getByText(/It is served by the same binary/)).toBeInTheDocument();
});

test("the step graph lists each step with its state and attempts, and a failure reason in full", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);

  const steps = await screen.findByRole("region", { name: /Steps/ });
  const items = within(steps).getAllByRole("listitem");
  expect(items.map((li) => within(li).getByRole("heading").textContent)).toEqual([
    "autobutler-org/factory/workspace",
    "autobutler-org/factory/implement",
    "autobutler-org/factory/gate",
    "autobutler-org/factory/open_pr",
  ]);

  const gate = items[2];
  expect(within(gate).getByText("failed")).toBeInTheDocument();
  const meta = within(gate).getByText(/after autobutler-org\/factory\/implement/);
  expect(meta).toHaveTextContent("gate · failed · 3 attempts");
  // The whole reason, several KB, not a truncated one, and open to read.
  const reason = within(gate).getByText(/failed after 3 attempts/);
  expect(reason.textContent).toContain(failedItem.gateOutput);
  expect(reason.closest("details")).toHaveAttribute("open");

  expect(within(items[3]).getByText("canceled")).toBeInTheDocument();
});

test("an item that is not planned yet says so", async () => {
  const routes = itemRoutes();
  delete routes[`/work/${ITEM_ID}/graph`];
  mockAPI(routes);
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);
  const steps = await screen.findByRole("region", { name: "Steps" });
  expect(within(steps).getByText("Not planned yet.")).toBeInTheDocument();
});

test("the timeline lists events oldest first with transitions and reasons", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);

  const timeline = await screen.findByRole("region", { name: "Timeline" });
  const events = within(timeline).getAllByRole("listitem");
  expect(events).toHaveLength(5);
  expect(events[0]).toHaveTextContent("created");
  expect(events[1]).toHaveTextContent("transition ready → in progress");
  expect(events[1]).toHaveTextContent("by worker worker-abc");
  expect(events[3]).toHaveTextContent("step result reported");
  expect(within(events[3]).getByText(/TestCase119/)).toBeInTheDocument();
  expect(events[4]).toHaveTextContent("in progress → failed");
});

test("a long reason scrolls in a region the keyboard can reach", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);
  const reason = await screen.findByRole("region", {
    name: "Reason for autobutler-org/factory/gate",
  });
  expect(reason).toHaveAttribute("tabindex", "0");
  expect(reason.textContent).toContain(failedItem.gateOutput);
});

test("a work item that does not exist says so, links to the board and stops asking", async () => {
  const fetch = mockAPI({});
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    renderWithAPI(<WorkItemPage id="nope" />);
    expect(
      await screen.findByRole("heading", { level: 1, name: "Work item not found" })
    ).toBeInTheDocument();
    expect(screen.getByText(/No work item has the ID/)).toHaveTextContent("nope");
    expect(screen.getByRole("link", { name: "Go to the board" })).toHaveAttribute("href", "/");
    expect(screen.queryByText("Loading…")).not.toBeInTheDocument();
    expect(document.title).toBe("Work item not found · factory");

    const calls = fetch.mock.calls.length;
    await vi.advanceTimersByTimeAsync(3 * POLL_MS);
    expect(fetch.mock.calls.length).toBe(calls);
  } finally {
    vi.useRealTimers();
  }
});

test("the page title is the item's title", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<WorkItemPage id={ITEM_ID} />);
  await screen.findByRole("heading", { level: 1, name: "build the factory web UI" });
  expect(document.title).toBe("build the factory web UI · factory");
});
