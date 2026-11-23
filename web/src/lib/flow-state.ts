import type { FlowRun, FlowDefinition, FlowNode } from "./flow-api";

export type FlowWire = { source: string; target: string; sourceHandle?: string | null };

// One stable output port can feed many branches, just like a Node-RED wire.
// Port identity must not depend on the position of a branch in an array.
export function flowWires(def: FlowDefinition) {
  return def.nodes.flatMap((node) => {
    const targets =
      node.type === "parallel"
        ? (node.branches ?? []).map((target) => ({ target, sourceHandle: "branches", label: "并行" }))
        : node.type === "branch"
          ? [
              { target: node.condition?.then, sourceHandle: "then", label: "成立" },
              { target: node.condition?.else, sourceHandle: "else", label: "不成立" },
            ]
          : [{ target: node.next, sourceHandle: undefined, label: "" }];
    return targets
      .filter((item) => item.target)
      .map((item) => ({
        id: `${node.id}:${item.sourceHandle ?? "next"}:${item.target}`,
        source: node.id,
        target: item.target!,
        sourceHandle: item.sourceHandle,
        label: item.label,
      }));
  });
}

function syncJoins(def: FlowDefinition): FlowDefinition {
  const wires = flowWires(def);
  return {
    ...def,
    nodes: def.nodes.map((node) =>
      node.type === "join"
        ? { ...node, from: [...new Set(wires.filter((wire) => wire.target === node.id).map((wire) => wire.source))] }
        : node,
    ),
  };
}

export function disconnectFlowWires(def: FlowDefinition, removed: FlowWire[]): FlowDefinition {
  return syncJoins({
    ...def,
    nodes: def.nodes.map((node) => {
      const matches = removed.filter((wire) => wire.source === node.id);
      if (!matches.length) return node;
      if (node.type === "parallel")
        return {
          ...node,
          branches: node.branches?.filter((target) => !matches.some((wire) => wire.target === target)),
        };
      if (node.type === "branch" && node.condition)
        return {
          ...node,
          condition: {
            ...node.condition,
            // biome-ignore lint/suspicious/noThenProperty: DSL conditional destination, not a promise callback.
            then: matches.some((wire) => wire.sourceHandle === "then" && wire.target === node.condition?.then)
              ? ""
              : node.condition.then,
            else: matches.some((wire) => wire.sourceHandle === "else" && wire.target === node.condition?.else)
              ? ""
              : node.condition.else,
          },
        };
      return matches.some((wire) => wire.target === node.next) ? { ...node, next: "" } : node;
    }),
  });
}

export function connectFlowNodes(def: FlowDefinition, wire: FlowWire): FlowDefinition {
  const source = def.nodes.find((node) => node.id === wire.source);
  const target = def.nodes.find((node) => node.id === wire.target);
  if (!source || !target) throw Error("连线的节点不存在");
  if (source.id === target.id || source.type === "end" || target.type === "start")
    throw Error("不能连接自身、从结束节点连出或连入开始节点");
  const wires = flowWires(def);
  const seen = new Set<string>();
  const reaches = (id: string): boolean => {
    if (id === source.id) return true;
    if (seen.has(id)) return false;
    seen.add(id);
    return wires.some((next) => next.source === id && reaches(next.target));
  };
  if (reaches(target.id)) throw Error("该连线会形成循环；当前工作流只支持有向无环流程");
  if (source.type === "parallel" && target.type === "join") throw Error("分叉应先连接执行节点，再将各分支末端接入汇合");
  if (source.type === "branch" && target.type === "join")
    throw Error("条件分支只执行一条路径，不能直接接入等待全部分支的汇合");
  return syncJoins({
    ...def,
    nodes: def.nodes.map((node) => {
      if (node.id !== source.id) return node;
      if (node.type === "parallel") return { ...node, branches: [...new Set([...(node.branches ?? []), target.id])] };
      if (node.type === "branch")
        return {
          ...node,
          condition: {
            ref: "input.target",
            equals: "",
            // biome-ignore lint/suspicious/noThenProperty: DSL conditional destination, not a promise callback.
            then: "",
            else: "",
            ...node.condition,
            [wire.sourceHandle === "else" ? "else" : "then"]: target.id,
          },
        };
      if (node.next && node.next !== target.id)
        throw Error("此节点已有下一步；需要同时调用多个工具时，请添加并行分叉，或拖动现有连线的末端重新连接");
      return { ...node, next: target.id };
    }),
  });
}

export function parallelDomainExample(): FlowDefinition {
  const ref = { $ref: "input.domain" };
  return {
    name: "parallel_domain",
    description: "同一域名并行交给两个命令，等待全部完成后汇总。示例仅回显，不发起扫描。",
    version: 1,
    input_schema: {
      type: "object",
      required: ["domain"],
      properties: { domain: { type: "string", default: "example.com" } },
    },
    entry: "begin",
    nodes: [
      { id: "begin", type: "start", next: "fork", position: { x: 0, y: 120 } },
      { id: "fork", type: "parallel", branches: ["dns", "http"], position: { x: 220, y: 120 } },
      ...["dns", "http"].map(
        (id, i): FlowNode => ({
          id,
          type: "tool",
          tool: "workflow_command",
          arguments: { program: "printf", argv: ["%s\\n", ref], parse: "lines" },
          next: "join",
          position: { x: 460, y: i * 230 },
        }),
      ),
      { id: "join", type: "join", from: ["dns", "http"], next: "result", position: { x: 720, y: 120 } },
      {
        id: "result",
        type: "end",
        output: { domain: ref, dns: { $ref: "steps.dns.data.value" }, http: { $ref: "steps.http.data.value" } },
        position: { x: 960, y: 120 },
      },
    ],
  };
}

// Layer by dependency depth so branches occupy separate rows instead of hiding
// behind one another. Explicit positions are replaced only on user request.
export function layoutFlow(def: FlowDefinition): FlowDefinition {
  const wires = flowWires(def);
  const depth = new Map<string, number>();
  const visiting = new Set<string>();
  function level(id: string): number {
    if (depth.has(id)) return depth.get(id)!;
    if (visiting.has(id)) return 0;
    visiting.add(id);
    const parents = wires.filter((wire) => wire.target === id);
    const value = parents.length ? 1 + Math.max(...parents.map((wire) => level(wire.source))) : 0;
    visiting.delete(id);
    depth.set(id, value);
    return value;
  }
  for (const node of def.nodes) level(node.id);
  const rows = new Map<number, number>();
  return {
    ...def,
    nodes: def.nodes.map((node) => {
      const x = depth.get(node.id)!;
      const row = rows.get(x) ?? 0;
      rows.set(x, row + 1);
      return { ...node, position: { x: 40 + x * 250, y: 50 + row * 180 } };
    }),
  };
}

// The initial HTTP snapshot can arrive after the SSE terminal event. A finished
// run must never revert to its earlier running snapshot.
export function reconcileFlowRun(current: FlowRun | null, incoming: FlowRun): FlowRun {
  if (current?.id === incoming.id && current.status !== "running" && incoming.status === "running") return current;
  return incoming;
}

export function renameFlowNode(def: FlowDefinition, from: string, to: string): FlowDefinition {
  if (!/^[a-zA-Z][a-zA-Z0-9_-]{0,58}$/.test(to))
    throw Error("节点 ID 需以字母开头，仅含字母、数字、下划线或短横线，最长 59 字符");
  if (from === to) return def;
  if (def.nodes.some((node) => node.id === to)) throw Error("节点 ID 已存在");
  const path = (value: string) =>
    value === `steps.${from}` || value.startsWith(`steps.${from}.`)
      ? `steps.${to}${value.slice(`steps.${from}`.length)}`
      : value === `#/steps/${from}` || value.startsWith(`#/steps/${from}/`)
        ? `#/steps/${to}${value.slice(`#/steps/${from}`.length)}`
        : value;
  const refs = (value: unknown): unknown => {
    if (Array.isArray(value)) return value.map(refs);
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [
          key,
          key === "$ref" && typeof item === "string" ? path(item) : refs(item),
        ]),
      );
    return value;
  };
  const id = (value: string) => (value === from ? to : value);
  return {
    ...def,
    entry: id(def.entry),
    nodes: def.nodes.map((node) => ({
      ...node,
      id: id(node.id),
      next: node.next ? id(node.next) : undefined,
      branches: node.branches?.map(id),
      from: node.from?.map(id),
      arguments: refs(node.arguments) as typeof node.arguments,
      output: refs(node.output),
      condition: node.condition
        ? {
            ...node.condition,
            ref: path(node.condition.ref),
            then: id(node.condition.then),
            else: id(node.condition.else),
          }
        : undefined,
    })),
  };
}

export function removeFlowNodes(def: FlowDefinition, ids: string[]): FlowDefinition {
  const removed = new Set(ids.filter((id) => id !== def.entry));
  return {
    ...def,
    nodes: def.nodes
      .filter((node) => !removed.has(node.id))
      .map((node) => ({
        ...node,
        next: node.next && removed.has(node.next) ? "" : node.next,
        branches: node.branches?.filter((id) => !removed.has(id)),
        from: node.from?.filter((id) => !removed.has(id)),
        condition: node.condition
          ? {
              ...node.condition,
              then: removed.has(node.condition.then) ? "" : node.condition.then,
              else: removed.has(node.condition.else) ? "" : node.condition.else,
            }
          : undefined,
      })),
  };
}
