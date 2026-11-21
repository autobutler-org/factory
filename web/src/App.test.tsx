import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { STATES } from "./api";
import { App } from "./App";
import { itemRoutes, ITEM_ID } from "./fixtures";
import { mockAPI, Reply } from "./testing";

const emptyBoard = Object.fromEntries(
  STATES.map((s) => [`/work?state=${s}&limit=200`, { items: [], next_cursor: "" }])
);
const unauthorized = new Reply(401, {
  error: { code: "unauthorized", message: "send the API token" },
});

beforeEach(() => window.history.replaceState(null, "", "/"));

test("asks for the token once, keeps it for the tab, and sends it", async () => {
  const fetch = mockAPI(emptyBoard);
  render(<App />);

  expect(screen.getByRole("heading", { name: "API token" })).toBeInTheDocument();
  expect(fetch).not.toHaveBeenCalled();
  await userEvent.type(screen.getByLabelText("Token"), "s3cret");
  await userEvent.click(screen.getByRole("button", { name: "Continue" }));

  expect(await screen.findByRole("heading", { name: "Board" })).toBeInTheDocument();
  expect(sessionStorage.getItem("factory.apiToken")).toBe("s3cret");
  expect(localStorage.length).toBe(0);
  expect(fetch.mock.calls[0][1]?.headers).toEqual({ Authorization: "Bearer s3cret" });
  // The token is never on the page.
  expect(document.body.textContent).not.toContain("s3cret");
});

test("a stored token skips the prompt", async () => {
  sessionStorage.setItem("factory.apiToken", "s3cret");
  mockAPI(emptyBoard);
  render(<App />);
  expect(await screen.findByRole("heading", { name: "Board" })).toBeInTheDocument();
  expect(screen.queryByLabelText("Token")).not.toBeInTheDocument();
});

test("a refused token asks again with a clear message", async () => {
  sessionStorage.setItem("factory.apiToken", "stale");
  const routes: Record<string, unknown> = Object.fromEntries(
    Object.keys(emptyBoard).map((k) => [k, unauthorized])
  );
  mockAPI(routes);
  render(<App />);

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "The server refused that token. Enter the current FACTORY_API_TOKEN."
  );
  expect(screen.getByLabelText("Token")).toBeInTheDocument();
  expect(sessionStorage.getItem("factory.apiToken")).toBeNull();

  // The right token gets the board.
  Object.assign(routes, emptyBoard);
  await userEvent.type(screen.getByLabelText("Token"), "fresh");
  await userEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { name: "Board" })).toBeInTheDocument();
});

test("links move between pages without reloading", async () => {
  sessionStorage.setItem("factory.apiToken", "s3cret");
  mockAPI({
    ...emptyBoard,
    [`/work?state=failed&limit=200`]: {
      items: [
        { id: ITEM_ID, title: "build the factory web UI", repos: [], stage: "", priority: 0 },
      ],
      next_cursor: "",
    },
    ...itemRoutes(),
    "/workers": { workers: [] },
  });
  render(<App />);

  await userEvent.click(await screen.findByRole("link", { name: "build the factory web UI" }));
  expect(window.location.pathname).toBe(`/work/${ITEM_ID}`);
  expect(await screen.findByRole("region", { name: "Timeline" })).toBeInTheDocument();

  await userEvent.click(screen.getByRole("link", { name: "Workers" }));
  expect(await screen.findByText("No worker has registered.")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Workers" })).toHaveAttribute("aria-current", "page");
});
