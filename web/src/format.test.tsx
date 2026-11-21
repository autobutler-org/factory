import { render, screen } from "@testing-library/react";
import { State, stateRole } from "./format";

test.each([
  // Work items
  ["design", "neutral"],
  ["ready", "neutral"],
  ["in_progress", "progress"],
  ["in_review", "progress"],
  ["blocked", "warning"],
  ["failed", "danger"],
  ["completed", "success"],
  ["wont_fix", "neutral"],
  // Steps
  ["pending", "neutral"],
  ["running", "progress"],
  ["succeeded", "success"],
  ["skipped", "neutral"],
  ["canceled", "neutral"],
  // Outputs
  ["open", "progress"],
  ["merged", "success"],
  ["closed", "neutral"],
  // Workers
  ["active", "success"],
  ["draining", "warning"],
  ["gone", "danger"],
  // A state this UI does not know yet
  ["something_new", "neutral"],
])("%s is %s", (state, role) => {
  expect(stateRole(state)).toBe(role);
});

test("a state is written out, not only colored", () => {
  render(<State state="in_progress" />);
  expect(screen.getByText("in progress")).toBeInTheDocument();
});
