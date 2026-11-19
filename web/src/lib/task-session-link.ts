// Task session links use persisted session keys, never reusable worker names.
export type TaskSessionLink = { session: string; seq?: number };

export function parseTaskSessionLink(search: string, taskId: string): TaskSessionLink | undefined {
  const query = new URLSearchParams(search);
  if (!query.has("session") && !query.has("activity")) return undefined;
  if (query.getAll("id").length !== 1 || query.get("id") !== taskId) {
    throw new Error("定位链接与当前任务不一致");
  }
  if (query.getAll("session").length !== 1 || query.getAll("activity").length > 1) {
    throw new Error("定位参数重复或缺少会话");
  }
  const session = query.get("session") ?? "";
  const match = /^(?:plan|main:(0|[1-9]\d*)|intent:([1-9]\d*))$/.exec(session);
  if (!match || (match[1] != null && !Number.isSafeInteger(Number(match[1]))) ||
      (match[2] != null && !Number.isSafeInteger(Number(match[2])))) {
    throw new Error("无效的会话定位参数");
  }
  const raw = query.get("activity");
  if (raw !== null && (!/^[1-9]\d*$/.test(raw) || !Number.isSafeInteger(Number(raw)))) {
    throw new Error("活动序号必须是正整数");
  }
  return { session, ...(raw !== null ? { seq: Number(raw) } : {}) };
}

// Walk the existing task-scoped intent endpoint without assuming the target is
// among the newest 300 workers. Cancellation also prevents stale-page followups.
export async function findLinkedIntent<T extends { id: string }>(
  id: string,
  load: (before: number) => Promise<{ items: T[]; hasMore: boolean }>,
  cancelled: () => boolean = () => false,
): Promise<T> {
  let before = 0;
  for (;;) {
    if (cancelled()) throw new Error("定位已取消");
    const page = await load(before);
    if (cancelled()) throw new Error("定位已取消");
    const found = page.items.find((item) => item.id === id);
    if (found) return found;
    if (!page.hasMore) throw new Error("当前任务中未找到对应 Worker，会话可能已删除");
    const next = Math.min(...page.items.map((item) => Number(item.id)));
    if (!Number.isSafeInteger(next) || next <= 0 || (before > 0 && next >= before)) {
      throw new Error("Worker 分页未前进，无法确认来源会话");
    }
    before = next;
  }
}

type LinkedActivity = { seq: number; kind: string; tool_use_id?: string };

// A provenance event can be a result, but ApprovalHistory expects the original
// call. Resolve by the persisted tool_use_id, never by adjacency or tool name.
export async function findLinkedToolCall<T extends LinkedActivity>(
  seq: number,
  load: (before: number) => Promise<{ items: T[]; hasMore: boolean }>,
  cancelled: () => boolean = () => false,
): Promise<T> {
  let before = 0;
  let target: T | undefined;
  const calls: T[] = [];
  for (;;) {
    if (cancelled()) throw new Error("定位已取消");
    const page = await load(before);
    if (cancelled()) throw new Error("定位已取消");
    for (const item of page.items) {
      if (item.seq === seq) target = item;
      if (item.kind === "tool_use" && item.tool_use_id) calls.push(item);
    }
    if (target?.kind === "tool_use") return target;
    if (target) {
      if (target.kind !== "tool_result") throw new Error("来源活动不是工具调用或执行结果");
      if (!target.tool_use_id) throw new Error("来源执行结果缺少工具调用标识，无法精确定位");
      // IDs may be reused by later executions. Only the nearest preceding
      // persisted call can own this result; never pair with a future call.
      const result = target;
      const call = calls.filter((item) => item.tool_use_id === result.tool_use_id && item.seq < result.seq)
        .sort((a, b) => b.seq - a.seq)[0];
      if (call) return call;
    }
    if (!page.hasMore || (!target && page.items.some((item) => item.seq < seq))) {
      throw new Error(target ? "未找到来源执行结果对应的工具调用" : "当前会话中未找到来源活动，记录可能已删除");
    }
    const next = Math.min(...page.items.map((item) => item.seq));
    if (!Number.isSafeInteger(next) || next <= 0 || (before > 0 && next >= before)) {
      throw new Error("活动分页未前进，无法精确定位");
    }
    before = next;
  }
}

export function withoutTaskSessionFocus(search: string): string {
  const query = new URLSearchParams(search);
  query.delete("session");
  query.delete("activity");
  return query.toString();
}
