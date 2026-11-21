import assert from "node:assert/strict";
import test from "node:test";
import { activityIdentity, projectStandardChat, projectTrajectory, recordedSystemPrompt } from "./projections.ts";

const event = (seq, kind, extra = {}) => ({
  seq,
  kind,
  summary: String(seq),
  worker: "worker",
  ts: "2026-10-31T00:00:00Z",
  ...extra,
});

test("parallel tools pair by call id, not adjacency, without replay duplicates", () => {
  const events = [
    event(1, "tool_use", { tool_use_id: "a" }),
    event(2, "tool_use", { tool_use_id: "b" }),
    event(3, "tool_result", { tool_use_id: "b", is_error: true }),
    event(4, "tool_result", { tool_use_id: "a" }),
  ];
  const records = projectTrajectory("task:1/main:0", [...events, events[3]]);
  assert.equal(records.length, 2);
  assert.equal(records[0].result.seq, 4);
  assert.equal(records[1].status, "error");
});

test("an orphan result retains its identity after loading the earlier tool call", () => {
  const call = event(2, "tool_use", { tool_use_id: "a" });
  const result = event(3, "tool_result", { tool_use_id: "a" });
  assert.equal(projectTrajectory("conv:1", [result])[0].id, projectTrajectory("conv:1", [call, result])[0].id);
});

test("equal sequence and tool ids in different sources or sessions never collide", () => {
  const a = event(1, "tool_use", { tool_use_id: "a", source_task_id: "1" });
  const b = event(1, "tool_result", { tool_use_id: "a", source_task_id: "2" });
  assert.equal(projectTrajectory("task:3", [a, b]).length, 2);
  assert.notEqual(activityIdentity("conv:1", a), activityIdentity("conv:2", a));
});

test("only turn boundaries settle process groups and model requests stay inside the chat process",  () => {
  const a = event(1, "thinking");
  const call = event(2, "model_call");
  assert.equal(projectStandardChat("conv:1", [a, call])[0].settled, false);
  const nodes = projectStandardChat("conv:1", [a, call, event(3, "result")]);
  assert.equal(nodes[0].settled, true);
  assert.deepEqual(nodes[0].events.map((entry) => entry.kind), ["thinking", "model_call"]);
  assert.equal(projectTrajectory("conv:1", [a, call]).length, 2);
});

test("a result without a later terminal remains pending instead of inventing completion", () => {
  assert.equal(projectTrajectory("task:1", [event(1, "tool_use", { tool_use_id: "a" })])[0].status, "pending");
});

test("steering or a new input does not manufacture a terminal event for prior work", () => {
  const nodes = projectStandardChat("task:1", [event(1, "thinking"), event(2, "user"), event(3, "result")]);
  assert.equal(nodes[0].settled, false);
});

test("chat retains message bubbles and groups execution details without flattening to a trajectory",  () => {
  const events = [
    event(1, "user"),
    event(2, "thinking"),
    event(3, "model_call", { metadata: { llm_call: { status: "ok" } } }),
    event(4, "tool_use", { tool_use_id: "a" }),
    event(5, "tool_result", { tool_use_id: "a" }),
    event(6, "text"),
    event(7, "usage"),
    event(8, "result"),
  ];
  const records = projectTrajectory("conv:1", events);
  assert.deepEqual(records.map((r) => r.event.kind), ["user", "thinking", "model_call", "tool_use", "text", "result"]);
  assert.equal(records[3].result.seq, 5);
  const nodes = projectStandardChat("conv:1", events);
  assert.deepEqual(nodes.map((node) => node.kind), ["message", "process", "message", "message"]);
  assert.equal(nodes[0].event.kind, "user");
  assert.deepEqual(nodes[1].events.map((entry) => entry.kind), ["thinking", "model_call", "tool_use", "tool_result"]);
  assert.equal(nodes[1].settled, true);
  assert.equal(nodes[2].event.kind, "text");
  assert.equal(nodes[3].event.kind, "result");
});

test("system prompts preserve captured text and distinguish missing history from an empty prompt", () => {
  const prompt = "  system instructions\\n\\nadditional instructions\\n";
  assert.equal(recordedSystemPrompt(JSON.stringify({ system_prompt: prompt })), prompt);
  assert.equal(recordedSystemPrompt('{"system_prompt":""}'), "");
  for (const detail of ["", "legacy text", "null", "{}", '{"system_prompt":42}', '{"system_prompt":null}']) {
    assert.equal(recordedSystemPrompt(detail), undefined);
  }
});

test("cancelled model requests remain interrupted rather than successful or failed", () => {
  const record = projectTrajectory("conv:1", [event(1, "model_call", { metadata: { llm_call: { status: "interrupted" } } })])[0];
  assert.equal(record.status, "interrupted");
});

 test("provider call IDs reused in later turns retain both executions and their own results", () => {
  const records=projectTrajectory("conv:1",[
   event(1,"tool_use",{tool_use_id:"reused"}), event(2,"tool_result",{tool_use_id:"reused"}),
   event(3,"tool_use",{tool_use_id:"reused"}), event(4,"tool_result",{tool_use_id:"reused",is_error:true}),
  ]);
  assert.equal(records.length,2);
  assert.notEqual(records[0].id,records[1].id);
  assert.equal(records[0].result.seq,2);
  assert.equal(records[1].result.seq,4);
  assert.equal(records[0].status,"done");
  assert.equal(records[1].status,"error");
 });
 test("a future call cannot steal an earlier orphan result with the same provider ID",()=>{
  const records=projectTrajectory("conv:1",[
   event(2,"tool_result",{tool_use_id:"reused"}),event(3,"tool_use",{tool_use_id:"reused"}),event(4,"tool_result",{tool_use_id:"reused"}),
  ]);
  assert.equal(records.length,2);
  assert.equal(records[0].event.kind,"tool_result");
  assert.equal(records[0].result.seq,2);
  assert.equal(records[1].event.seq,3);
  assert.equal(records[1].result.seq,4);
 });

test("cumulative usage cannot evict recorded execution steps", async () => {
  const { compactUsage } = await import("../../lib/activity-merge.ts");
  const steps = [event(1, "user"), event(2, "thinking"), event(3, "tool_use", {tool_use_id:"a"}), event(4, "tool_result", {tool_use_id:"a"})];
  const flood = Array.from({length: 2000}, (_, i) => event(i + 5, "usage", {output_tokens: i}));
  const compacted = compactUsage([...steps, ...flood, event(2005, "result")]);
  assert.deepEqual(compacted.map(e => e.kind), ["user", "thinking", "tool_use", "tool_result", "usage", "result"]);
  assert.equal(compacted.find(e => e.kind === "usage").output_tokens, 1999);
  assert.equal(projectTrajectory("task:1", compacted).filter(r => r.event.kind === "tool_use").length, 1);
});
