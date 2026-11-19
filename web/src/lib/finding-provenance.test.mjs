import { findingEdgeSessionLabel, sourcePrecisionLabel, sourceSessionHref } from "./finding-provenance.ts";
import assert from "node:assert/strict";
import test from "node:test";

const source = { task_id: "9", session: "intent:123", title: "Worker #123", role: "worker", contributions: [] };
const contribution = { node_id: "456", relation: "produced", summary: "发现", precision: "event", seq: 12345 };

test("source links preserve exact task, session and activity identity", () => {
  const url = new URL(sourceSessionHref(source, contribution), "http://localhost");
  assert.equal(url.pathname, "/function/tasks/detail");
  assert.equal(url.searchParams.get("id"), "9");
  assert.equal(url.searchParams.get("session"), "intent:123");
  assert.equal(url.searchParams.get("activity"), "12345");
});

test("session-only and legacy links never promise an event position", () => {
  for (const precision of ["legacy", "session"]) {
    assert.equal(
      new URL(sourceSessionHref(source, { ...contribution, precision }), "http://localhost").searchParams.has(
        "activity",
      ),
      false,
    );
  }
  assert.match(sourcePrecisionLabel({ ...contribution, precision: "legacy" }), /实际提交会话未记录/);
});

test("invalid event ids do not produce a misleading focus URL", () => {
  for (const seq of [undefined, 0, -1, NaN, Infinity, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal(
      new URL(sourceSessionHref(source, { ...contribution, seq }), "http://localhost").searchParams.has("activity"),
      false,
    );
  }
});

test("main session segments and planner remain distinct", () => {
  for (const session of ["main:0", "main:3", "plan"]) {
    const url = new URL(sourceSessionHref({ ...source, session }), "http://localhost");
    assert.equal(url.searchParams.get("session"), session);
    assert.equal(url.searchParams.has("activity"), false);
  }
});

const session = (key, contributions) => ({ ...source, session: key, contributions });
const c = (node, relation, precision = "event") => ({ ...contribution, node_id: node, relation, precision });

test("edge labels show destination creation/execution without leaking unrelated sessions", () => {
  const edge = { src: "1", dst: "2", rel: "derived_from" };
  const label = findingEdgeSessionLabel(
    [edge],
    [
      session("main:0", [c("2", "created")]),
      session("plan", [c("2", "created")]),
      session("intent:2", [c("2", "executed", "session")]),
      session("intent:9", [c("9", "executed", "session")]),
    ],
  );
  assert.equal(label, "创建：主会话 #1 · 创建：规划会话 · 执行：意图会话 #2");
});

test("actual submitter wins over a logical yields source and duplicate events do not expand labels", () => {
  const label = findingEdgeSessionLabel(
    [{ src: "1", dst: "3", rel: "yields" }],
    [session("main:3", [c("3", "produced"), c("3", "produced")]), session("intent:8", [c("3", "verified")])],
  );
  assert.equal(label, "产出：主会话 #4 · 复验：意图会话 #8");
  assert.doesNotMatch(label, /意图会话 #1/);
});

test("legacy labels are uncertain and limited to the exact yields edge", () => {
  const sessions = [session("intent:1", [c("3", "produced", "legacy")])];
  assert.equal(findingEdgeSessionLabel([{ src: "1", dst: "3", rel: "yields" }], sessions), "历史关联：意图会话 #1");
  for (const edge of [
    { src: "8", dst: "3", rel: "yields" },
    { src: "1", dst: "3", rel: "derived_from" },
  ]) {
    assert.equal(findingEdgeSessionLabel([edge], sessions), "会话未记录");
  }
  assert.equal(findingEdgeSessionLabel([{ src: "1", dst: "3", rel: "yields" }], []), "会话未记录");
});

test("folded edge labels aggregate all original destinations without duplication", () => {
  const label = findingEdgeSessionLabel(
    [
      { src: "1", dst: "2", rel: "yields" },
      { src: "1", dst: "3", rel: "yields" },
    ],
    [session("intent:1", [c("2", "produced"), c("3", "produced")]), session("main:2", [c("3", "produced")])],
  );
  assert.equal(label, "产出：意图会话 #1 · 产出：主会话 #3");
});
