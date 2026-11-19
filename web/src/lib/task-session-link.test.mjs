import assert from "node:assert/strict";
import test from "node:test";
import { findLinkedIntent, findLinkedToolCall, parseTaskSessionLink, withoutTaskSessionFocus } from "./task-session-link.ts";

const parse = (query) => parseTaskSessionLink(query, "TASK");
test("decodes persisted intent, main segment zero/older, and planner keys", () => {
  assert.deepEqual(parse("?id=TASK&session=intent%3A42&activity=19"), { session: "intent:42", seq: 19 });
  assert.deepEqual(parse("?id=TASK&session=main:0"), { session: "main:0" });
  assert.deepEqual(parse("?id=TASK&session=main:12"), { session: "main:12" });
  assert.deepEqual(parse("?id=TASK&session=plan"), { session: "plan" });
  assert.equal(parse("?id=TASK"), undefined);
});
test("rejects cross-task, missing, duplicate, malformed, and unsafe parameters", () => {
  for (const query of [
    "?id=OTHER&session=plan", "?session=plan", "?id=TASK&activity=1",
    "?id=TASK&session=plan&session=main:0", "?id=TASK&session=plan&activity=1&activity=2",
    ...["main", "main:-1", "main:01", "main:1.2", "main:9007199254740992", "intent:", "intent:0", "intent:-1", "intent:abc", "intent:9007199254740992", "system", ""].map((s) => `?id=TASK&session=${s}`),
    ...["", "0", "-1", "1.5", "1e2", "+1", " 1", "01", "Infinity", "9007199254740992"].map((seq) => `?id=TASK&session=plan&activity=${seq}`),
  ]) assert.throws(() => parse(query), query);
});
test("router query changes resolve independently, and cancel preserves unrelated query", () => {
  assert.notDeepEqual(parse("?id=TASK&session=intent:2"), parse("?id=TASK&session=main:3&activity=4"));
  assert.equal(withoutTaskSessionFocus("?id=TASK&session=intent:2&activity=9&tab=sessions"), "id=TASK&tab=sessions");
});
test("finds a worker beyond the first current page without loading graph", async () => {
  const seen = [];
  const node = await findLinkedIntent("8", async (before) => {
    seen.push(before);
    return before === 0 ? { items: [{ id: "999" }, { id: "700" }], hasMore: true }
      : { items: [{ id: "8", state: "deleted" }], hasMore: false };
  });
  assert.equal(node.id, "8");
  assert.deepEqual(seen, [0, 700]);
});
test("missing workers, network errors and nonadvancing pages never pick a fallback", async () => {
  await assert.rejects(findLinkedIntent("1", async () => ({ items: [], hasMore: false })), /未找到/);
  await assert.rejects(findLinkedIntent("1", async () => ({ items: [], hasMore: true })), /未前进/);
  await assert.rejects(findLinkedIntent("1", async () => ({ items: [{ id: "2" }], hasMore: true })), /未前进/);
  await assert.rejects(findLinkedIntent("1", async () => { throw new Error("offline"); }), /offline/);
});
test("query/task cancellation stops stale worker pagination", async () => {
  let cancelled = false;
  let calls = 0;
  await assert.rejects(findLinkedIntent("1", async () => {
    calls++;
    cancelled = true;
    return { items: [{ id: "2" }], hasMore: true };
  }, () => cancelled), /取消/);
  assert.equal(calls, 1);
});
const event = (seq, kind = "tool_use", tool_use_id = "call-A") => ({ seq, kind, tool_use_id });
test("resolves exact tool call despite transcript pagination", async () => {
  const seen = [];
  const found = await findLinkedToolCall(5, async (before) => {
    seen.push(before);
    return before === 0 ? { items: [event(20)], hasMore: true } : { items: [event(5)], hasMore: false };
  });
  assert.equal(found.seq, 5);
  assert.deepEqual(seen, [0, 20]);
});
test("result provenance resolves its paired call by tool_use_id across pages", async () => {
  const found = await findLinkedToolCall(10, async (before) => before === 0
    ? { items: [event(10, "tool_result"), event(11, "tool_use", "another")], hasMore: true }
    : { items: [event(5), event(9, "tool_use", "another")], hasMore: false });
  assert.equal(found.seq, 5);
});
test("a reused ID after the target result cannot steal the focus", async () => {
  const seen = [];
  const found = await findLinkedToolCall(10, async (before) => {
    seen.push(before);
    return before === 0
      ? { items: [event(10, "tool_result"), event(11), event(12, "tool_result")], hasMore: true }
      : { items: [event(2), event(5)], hasMore: false };
  });
  assert.equal(found.seq, 5);
  assert.deepEqual(seen, [0, 10]);
  await assert.rejects(findLinkedToolCall(10, async () => ({
    items: [event(10, "tool_result"), event(11)], hasMore: false,
  })), /对应的工具调用/);
});
test("missing/wrong activity, unpaired results, bad pagination and failed loads are explicit", async () => {
  await assert.rejects(findLinkedToolCall(7, async () => ({ items: [event(5)], hasMore: true })), /未找到来源活动/);
  await assert.rejects(findLinkedToolCall(7, async () => ({ items: [event(7, "text")], hasMore: false })), /不是工具/);
  await assert.rejects(findLinkedToolCall(7, async () => ({ items: [event(7, "tool_result", "")], hasMore: false })), /缺少/);
  await assert.rejects(findLinkedToolCall(7, async () => ({ items: [event(7, "tool_result"), event(6, "tool_use", "other")], hasMore: false })), /对应的工具调用/);
  await assert.rejects(findLinkedToolCall(7, async () => ({ items: [], hasMore: true })), /未前进/);
  await assert.rejects(findLinkedToolCall(7, async () => { throw new Error("offline"); }), /offline/);
});
test("query/task cancellation also prevents stale activity resolution", async () => {
  let cancelled = false;
  await assert.rejects(findLinkedToolCall(7, async () => {
    cancelled = true;
    return { items: [event(7)], hasMore: false };
  }, () => cancelled), /取消/);
});
