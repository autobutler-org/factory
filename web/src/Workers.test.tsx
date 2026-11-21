import { screen, within } from "@testing-library/react";
import { mockAPI, renderWithAPI } from "./testing";
import { Workers } from "./Workers";

test("workers show their capabilities, last seen and what they hold", async () => {
  mockAPI({
    "/workers": {
      workers: [
        {
          id: "w1",
          name: "worker-3f0c8a52",
          kind: "cluster",
          capabilities: { harnesses: ["claude"], labels: ["arm64"] },
          version: "v0.2.0",
          registered_at: "2026-11-20T10:00:00Z",
          last_seen_at: new Date(Date.now() - 5 * 60_000).toISOString(),
          state: "active",
          holds: [
            { work_item_id: "i1", title: "add the web UI", step_id: "s1", step_key: "o/r/gate" },
          ],
        },
        {
          id: "w2",
          name: "laptop",
          kind: "local",
          capabilities: {},
          version: "",
          registered_at: "2026-11-19T10:00:00Z",
          last_seen_at: "2026-11-19T10:00:00Z",
          state: "gone",
          holds: [],
        },
      ],
    },
  });
  renderWithAPI(<Workers />);

  const cards = await screen.findAllByRole("listitem");
  const cluster = cards[0];
  expect(within(cluster).getByRole("heading", { name: "worker-3f0c8a52" })).toBeInTheDocument();
  expect(within(cluster).getByText("claude, arm64")).toBeInTheDocument();
  expect(within(cluster).getByText(/5m ago/)).toBeInTheDocument();
  expect(within(cluster).getByRole("link", { name: "add the web UI" })).toHaveAttribute(
    "href",
    "/work/i1"
  );
  expect(within(cluster).getByText(/step o\/r\/gate/)).toBeInTheDocument();

  const laptop = screen.getByRole("heading", { name: "laptop" }).closest("li")!;
  expect(within(laptop).getByText("gone")).toBeInTheDocument();
  expect(within(laptop).getByText("nothing")).toBeInTheDocument();
});
