import type { FlowRun } from "./flow-api";

// The initial HTTP snapshot can arrive after the SSE terminal event. A finished
// run must never revert to its earlier running snapshot.
export function reconcileFlowRun(current: FlowRun | null, incoming: FlowRun): FlowRun {
  if (current?.id === incoming.id && current.status !== "running" && incoming.status === "running") return current;
  return incoming;
}
