import type { Activity } from "./types";

/** Full message revisions are idempotent across history, replay and live delivery. */
export function mergeActivities(current: Activity[], incoming: Activity[]): Activity[] {
  const messages = new Map<string, Activity>();
  for (const event of [...current, ...incoming]) {
    const key = JSON.stringify([event.source_task_id ?? "local", event.message_id ?? event.seq]);
    const previous = messages.get(key);
    if (
      !previous ||
      (event.revision ?? 0) > (previous.revision ?? 0) ||
      ((event.revision ?? 0) === (previous.revision ?? 0) && event.seq >= previous.seq)
    ) {
      messages.set(key, event);
    }
  }
  return compactUsage([...messages.values()].sort((a, b) => a.seq - b.seq));
}

// Usage is cumulative within a run. Retain only the newest sample in each
// session; token frames must not evict visible execution steps from the UI cap.
export function compactUsage(events: Activity[]): Activity[] {
  const latest = new Map<string, Activity>();
  for (const event of events) {
    if (event.kind === "usage") latest.set(JSON.stringify([event.source_task_id ?? "local", event.worker, event.intent_id, event.main_seg]), event);
  }
  return events.filter((event) => event.kind !== "usage" || latest.get(JSON.stringify([event.source_task_id ?? "local", event.worker, event.intent_id, event.main_seg])) === event);
}
