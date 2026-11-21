import { screen, within } from "@testing-library/react";
import { failedItem, itemRoutes, ITEM_ID } from "./fixtures";
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
