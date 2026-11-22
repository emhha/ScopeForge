import type { FlowRun, FlowDefinition } from "./flow-api";

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
