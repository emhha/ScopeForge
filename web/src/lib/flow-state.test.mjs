import assert from "node:assert/strict";
import test from "node:test";
import { reconcileFlowRun } from "./flow-state.ts";

test("a late HTTP snapshot cannot overwrite the SSE result", () => {
  const initial = { id: "run-one", status: "running", result: "" };
  const completed = { ...initial, status: "completed", result: "accepted" };
  assert.deepEqual(reconcileFlowRun(initial, completed), completed);
  assert.deepEqual(reconcileFlowRun(completed, initial), completed);
  const next = { id: "run-two", status: "running", result: "" };
  assert.deepEqual(reconcileFlowRun(completed, next), next);
});
