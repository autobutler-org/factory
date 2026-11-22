import { screen, waitFor } from "@testing-library/react";
import { failedItem, itemRoutes, ITEM_ID } from "./fixtures";
import { StepOutputPage, stepOutputPath } from "./StepOutput";
import { mockAPI, renderWithAPI, Reply, Text } from "./testing";

const outputRoute = "/steps/step-gate/output";

test("the page shows a step's whole gate output in a named region", async () => {
  mockAPI({ ...itemRoutes(), [outputRoute]: new Text(failedItem.gateOutput) });
  renderWithAPI(<StepOutputPage id={ITEM_ID} stepID="step-gate" />);

  expect(
    await screen.findByRole("heading", {
      level: 1,
      name: "Gate output of autobutler-org/factory/gate",
    })
  ).toBeInTheDocument();
  const region = await screen.findByRole("region", {
    name: "Gate output of autobutler-org/factory/gate",
  });
  expect(region.textContent).toBe(failedItem.gateOutput);
  expect(
    screen.getByText(failedItem.gateOutput.length.toLocaleString()).closest("p")
  ).toHaveTextContent("bytes · stored");
  expect(screen.getByRole("link", { name: "Back to the work item" })).toHaveAttribute(
    "href",
    `/work/${ITEM_ID}`
  );
  await waitFor(() =>
    expect(document.title).toBe("Gate output of autobutler-org/factory/gate · factory")
  );
});

test("a step without a kept output says why", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<StepOutputPage id={ITEM_ID} stepID="step-implement" />);
  expect(await screen.findByText(/No gate output is kept for this step/)).toBeInTheDocument();
});

test("a step that is not in the latest plan says so", async () => {
  mockAPI(itemRoutes());
  renderWithAPI(<StepOutputPage id={ITEM_ID} stepID="step-gone" />);
  expect(await screen.findByText(/has no step with the ID/)).toBeInTheDocument();
});

test("an output that cannot be loaded reports the error", async () => {
  mockAPI({
    ...itemRoutes(),
    [outputRoute]: new Reply(500, { error: { code: "internal", message: "internal error" } }),
  });
  renderWithAPI(<StepOutputPage id={ITEM_ID} stepID="step-gate" />);
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent("Could not load: internal error")
  );
});

test("an item that does not exist says so instead of a blank page", async () => {
  mockAPI({});
  renderWithAPI(<StepOutputPage id="nope" stepID="step-gate" />);
  expect(
    await screen.findByText(/Nothing to show: the work item does not exist/)
  ).toBeInTheDocument();
});

test("a 401 hands back to the token prompt", async () => {
  mockAPI({
    [`/work/${ITEM_ID}/graph`]: new Reply(401, { error: { code: "unauthorized", message: "no" } }),
  });
  const refused = vi.fn();
  renderWithAPI(<StepOutputPage id={ITEM_ID} stepID="step-gate" />, refused);
  await waitFor(() => expect(refused).toHaveBeenCalled());
});

test("stepOutputPath escapes the IDs", () => {
  expect(stepOutputPath("a/b", "c d")).toBe("/work/a%2Fb/steps/c%20d/output");
});
