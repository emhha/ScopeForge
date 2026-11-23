import assert from "node:assert/strict";
import test from "node:test";
import {
  reconcileFlowRun,
  flowWires,
  connectFlowNodes,
  disconnectFlowWires,
  parallelDomainExample,
  renameFlowNode,
  removeFlowNodes,
  layoutFlow,
} from "./flow-state.ts";

test("a late HTTP snapshot cannot overwrite the SSE result", () => {
  const initial = { id: "run-one", status: "running", result: "" };
  const completed = { ...initial, status: "completed", result: "accepted" };
  assert.deepEqual(reconcileFlowRun(initial, completed), completed);
  assert.deepEqual(reconcileFlowRun(completed, initial), completed);
  const next = { id: "run-two", status: "running", result: "" };
  assert.deepEqual(reconcileFlowRun(completed, next), next);
});

test("parallel fan-out uses one stable port as branches are added and deleted", () => {
  let def = parallelDomainExample();
  def = disconnectFlowWires(
    def,
    flowWires(def).filter((wire) => wire.source === "fork"),
  );
  for (const target of ["dns", "http"])
    def = connectFlowNodes(def, { source: "fork", target, sourceHandle: "branches" });
  let wires = flowWires(def).filter((wire) => wire.source === "fork");
  assert.equal(wires.length, 2);
  assert(wires.every((wire) => wire.sourceHandle === "branches"));
  const retainedID = wires[1].id;
  def = disconnectFlowWires(def, [wires[0]]);
  wires = flowWires(def).filter((wire) => wire.source === "fork");
  assert.equal(wires[0].id, retainedID);
  assert.equal(wires[0].sourceHandle, "branches");
});

test("reconnecting and deleting edges keeps joins consistent with visible wires", () => {
  let def = parallelDomainExample();
  const old = flowWires(def).find((wire) => wire.source === "dns");
  def = disconnectFlowWires(def, [old]);
  assert.deepEqual(def.nodes.find((node) => node.id === "join").from, ["http"]);
  def = connectFlowNodes(def, { source: "dns", target: "join" });
  assert.deepEqual(new Set(def.nodes.find((node) => node.id === "join").from), new Set(["dns", "http"]));
  // Delete both conditional ports at once, including when they share a target.
  def.nodes.push({
    id: "choose",
    type: "branch",
    condition: { ref: "input.domain", equals: "", then: "result", else: "result" },
  });
  def = disconnectFlowWires(
    def,
    flowWires(def).filter((wire) => wire.source === "choose"),
  );
  assert.equal(def.nodes.at(-1).condition.then, "");
  assert.equal(def.nodes.at(-1).condition.else, "");
});

test("invalid connections do not erase an existing edge or introduce cycles", () => {
  const def = parallelDomainExample();
  assert.throws(() => connectFlowNodes(def, { source: "dns", target: "http" }), /已有下一步/);
  assert.throws(() => connectFlowNodes(def, { source: "join", target: "fork" }), /循环/);
  assert.throws(() => connectFlowNodes(def, { source: "dns", target: "begin" }), /开始节点/);
  assert.throws(() => connectFlowNodes(def, { source: "result", target: "dns" }), /结束节点/);
  assert.equal(def.nodes.find((node) => node.id === "dns").next, "join");
});

test("renaming or removing a parallel node preserves references and join membership", () => {
  let def = renameFlowNode(parallelDomainExample(), "dns", "resolve");
  assert.deepEqual(def.nodes.find((node) => node.id === "fork").branches, ["resolve", "http"]);
  assert.equal(def.nodes.at(-1).output.dns.$ref, "steps.resolve.data.value");
  def = removeFlowNodes(def, ["resolve"]);
  assert.deepEqual(def.nodes.find((node) => node.id === "join").from, ["http"]);
  assert.deepEqual(def.nodes.find((node) => node.id === "fork").branches, ["http"]);
});

test("domain template binds the same input to both commands and lays out distinct branches", () => {
  const def = layoutFlow(parallelDomainExample());
  const left = def.nodes.find((node) => node.id === "dns");
  const right = def.nodes.find((node) => node.id === "http");
  assert.deepEqual(left.arguments.argv[1], { $ref: "input.domain" });
  assert.deepEqual(right.arguments.argv[1], left.arguments.argv[1]);
  assert.notEqual(left.position.y, right.position.y);
  assert.equal(left.position.x, right.position.x);
  assert(def.nodes.find((node) => node.id === "join").position.x > left.position.x);
});
