import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { getter } from "./api";
import { ApiContext } from "./poll";

// Reply is a non-200 answer; any other route value is a 200 with that JSON.
export class Reply {
  readonly status: number;
  readonly body: unknown;
  constructor(status: number, body: unknown) {
    this.status = status;
    this.body = body;
  }
}

// mockAPI answers fetch from routes keyed by path and query (without /api/v1); anything else is a
// 404. It returns the mock so a test can read the calls.
export function mockAPI(routes: Record<string, unknown>) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = new URL(String(input), "http://factory.test");
    const key = url.pathname.replace(/^\/api\/v1/, "") + url.search;
    const hit = routes[key];
    const reply =
      hit === undefined
        ? new Reply(404, { error: { code: "not_found", message: "no " + key } })
        : hit instanceof Reply
          ? hit
          : new Reply(200, hit);
    return new Response(JSON.stringify(reply.body), {
      status: reply.status,
      headers: { "Content-Type": "application/json" },
    });
  });
}

export function renderWithAPI(ui: ReactNode, refused = () => {}) {
  return render(
    <ApiContext.Provider value={{ get: getter("t0ken"), refused }}>{ui}</ApiContext.Provider>
  );
}
