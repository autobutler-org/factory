import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { STATES } from "./api";
import { Board } from "./Board";
import { mockAPI, renderWithAPI } from "./testing";

const item = (id: string, title: string, state: string, extra = {}) => ({
  id,
  title,
  state,
  repos: ["autobutler-org/factory"],
  stage: "",
  priority: 0,
  ...extra,
});

function routes(query = "") {
  const r: Record<string, unknown> = {};
  for (const s of STATES) r[`/work?state=${s}&limit=200${query}`] = { items: [], next_cursor: "" };
  r[`/work?state=ready&limit=200${query}`] = {
    items: [item("a", "add the web UI", "ready"), item("b", "rotate certificates", "ready")],
    next_cursor: "",
  };
  r[`/work?state=in_progress&limit=200${query}`] = {
    items: [item("c", "port the code host", "in_progress", { stage: "implement" })],
    next_cursor: "",
  };
  r[`/work?state=completed&limit=200${query}`] = {
    items: [item("d", "old work", "completed")],
    next_cursor: "more",
  };
  return r;
}

test("the board shows a column per state with counts and links to each item", async () => {
  mockAPI(routes());
  renderWithAPI(<Board />);

  const ready = await screen.findByRole("region", { name: /ready/i });
  expect(within(ready).getByRole("heading", { name: "ready 2" })).toBeInTheDocument();
  expect(within(ready).getByRole("link", { name: "add the web UI" })).toHaveAttribute(
    "href",
    "/work/a"
  );

  const working = screen.getByRole("region", { name: /in progress/i });
  expect(within(working).getByText(/implement/)).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "completed 1+" })).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "design 0" })).toBeInTheDocument();
  expect(screen.getAllByRole("region")).toHaveLength(STATES.length);
});

test("filtering by repository queries the API with it", async () => {
  const fetch = mockAPI({ ...routes(), ...routes("&repo=o%2Fr") });
  renderWithAPI(<Board />);
  await screen.findByRole("region", { name: /ready/i });

  await userEvent.type(screen.getByRole("textbox", { name: "Repository" }), "o/r");
  await userEvent.click(screen.getByRole("button", { name: "Filter" }));

  await vi.waitFor(() =>
    expect(fetch).toHaveBeenCalledWith(
      "/api/v1/work?state=ready&limit=200&repo=o%2Fr",
      expect.anything()
    )
  );
});

test("every request carries the token", async () => {
  const fetch = mockAPI(routes());
  renderWithAPI(<Board />);
  await screen.findByRole("region", { name: /ready/i });
  for (const [, init] of fetch.mock.calls) {
    expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer t0ken");
  }
});
