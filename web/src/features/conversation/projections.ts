import type { Activity } from "../../lib/types.ts";

export function activityIdentity(scope: string, event: Activity): string {
  return JSON.stringify([scope, event.source_task_id ?? "local", event.seq]);
}

function callIdentity(scope: string, event: Activity): string {
  return JSON.stringify([
    scope,
    event.source_task_id ?? "local",
    event.intent_id ?? `main:${event.main_seg ?? 0}`,
    event.tool_use_id,
  ]);
}

export interface TrajectoryRecord {
  id: string;
  event: Activity;
  result?: Activity;
  status: "done" | "error" | "interrupted" | "pending" | "recorded";
}

function recordStatus(event: Activity, result?: Activity): TrajectoryRecord["status"] {
  if (event.is_error || result?.is_error || event.metadata?.llm_call?.status === "error") return "error";
  if (event.metadata?.llm_call?.status === "interrupted") return "interrupted";
  if (event.kind === "tool_use" && !result) return "pending";
  if (result || ["result", "tool_result", "model_call"].includes(event.kind)) return "done";
  return "recorded";
}

// Pair by call identity, including the source task. History may start with an
// orphan result and later prepend its call; the record identity still survives.
export function projectTrajectory(scope: string, events: Activity[]): TrajectoryRecord[] {
  const records: TrajectoryRecord[] = [];
  const calls = new Map<string, TrajectoryRecord>();
  const seen = new Set<string>();
  for (const event of events) {
    const identity = activityIdentity(scope, event);
    if (seen.has(identity) || event.kind === "usage") continue;
    seen.add(identity);
    const tool = event.kind === "tool_use" || event.kind === "tool_result";
    const key = tool && event.tool_use_id ? `tool:${callIdentity(scope, event)}` : identity;
    if (tool && event.tool_use_id) {
      const existing = calls.get(key);
      const canPair = existing && (
        event.kind === "tool_result"
          ? !existing.result && existing.event.kind === "tool_use" && existing.event.seq < event.seq
          : existing.event.kind === "tool_result" && event.seq < existing.event.seq
      );
      if (canPair) {
        if (event.kind === "tool_result") existing.result = event;
        else existing.event = event;
        existing.status = recordStatus(existing.event, existing.result);
        continue;
      }
    }
    const record: TrajectoryRecord = {
      // A later model turn may reuse a provider call ID. Keep each execution.
      id: calls.has(key) ? `${key}:${event.seq}` : key,
      event,
      result: event.kind === "tool_result" ? event : undefined,
      status: recordStatus(event),
    };
    records.push(record);
    if (tool && event.tool_use_id) calls.set(key, record);
  }
  return records;
}

/** Read only the captured request prompt, never substitute current agent settings. */
export function recordedSystemPrompt(detail: string): string | undefined {
  try {
    const value: unknown = JSON.parse(detail);
    if (value && typeof value === "object" && "system_prompt" in value && typeof value.system_prompt === "string") {
      return value.system_prompt;
    }
  } catch {
    // Older activity details may be plain text or absent.
  }
  return undefined;
}

export type ChatNode =
  | { kind: "message"; id: string; event: Activity }
  | { kind: "process"; id: string; events: Activity[]; settled: boolean };

export function projectStandardChat(scope: string, events: Activity[]): ChatNode[] {
  const nodes: ChatNode[] = [];
  let process: Extract<ChatNode, { kind: "process" }> | undefined;
  const pending: Extract<ChatNode, { kind: "process" }>[] = [];
  const seen = new Set<string>();
  for (const event of events) {
    const id = activityIdentity(scope, event);
    if (seen.has(id) || event.kind === "usage") continue;
    seen.add(id);
    if (["thinking", "tool_use", "tool_result", "model_call"].includes(event.kind)) {
      if (!process) {
        process = { kind: "process", id, events: [], settled: false };
        nodes.push(process);
        pending.push(process);
      }
      process.events.push(event);
      continue;
    }
    // Only recorded turn boundaries settle work, never an SSE idle timeout.
    if (event.kind === "result") {
      for (const group of pending) group.settled = true;
      pending.length = 0;
    } else if (event.kind === "user" || event.kind === "round") {
      // A steering message can arrive while the earlier work is still running.
      // A structural boundary is not evidence that the previous turn completed.
      pending.length = 0;
    }
    process = undefined;
    nodes.push({ kind: "message", id, event });
  }
  return nodes;
}
