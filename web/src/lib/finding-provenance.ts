import type { Edge, FindingContribution, FindingSourceSession } from "./types";

export const contributionLabels: Record<FindingContribution["relation"], string> = {
  created: "创建",
  executed: "执行",
  produced: "产出",
  verified: "复验",
};

export function sourceSessionHref(source: FindingSourceSession, contribution?: FindingContribution): string {
  const q = new URLSearchParams({ id: source.task_id, session: source.session });
  if (
    contribution?.precision === "event" &&
    typeof contribution.seq === "number" &&
    Number.isSafeInteger(contribution.seq) &&
    contribution.seq > 0
  ) {
    q.set("activity", String(contribution.seq));
  }
  return `/function/tasks/detail?${q}`;
}

// Use stable session identities, not a reusable worker name or an unbounded intent summary.
function compactSessionName(source: FindingSourceSession): string {
  if (source.session === "plan") return "规划会话";
  const main = /^main:(\d+)$/.exec(source.session);
  if (main && Number.isSafeInteger(Number(main[1]) + 1)) return `主会话 #${Number(main[1]) + 1}`;
  const intent = /^intent:(\d+)$/.exec(source.session);
  if (intent) return `意图会话 #${intent[1]}`;
  return source.session;
}

// The label describes recorded contributions to each original edge's destination.
// It does NOT infer an actor from the edge itself. Folded edges retain their
// originals so digest rewiring cannot lose or misattribute the source sessions.
export function findingEdgeSessionLabel(edges: Edge[], sessions: FindingSourceSession[]): string {
  const labels = new Set<string>();
  for (const edge of edges) {
    for (const source of sessions) {
      for (const contribution of source.contributions) {
        if (contribution.node_id !== edge.dst) continue;
        if (contribution.precision === "legacy") {
          if (edge.rel === "yields" && source.session === `intent:${edge.src}`) {
            labels.add(`历史关联：${compactSessionName(source)}`);
          }
          continue;
        }
        labels.add(`${contributionLabels[contribution.relation]}：${compactSessionName(source)}`);
      }
    }
  }
  return [...labels].join(" · ") || "会话未记录";
}

export function sourcePrecisionLabel(contribution: FindingContribution): string {
  if (contribution.precision === "event") return "已记录来源事件";
  if (contribution.precision === "legacy") return "历史链路关联 · 实际提交会话未记录";
  return "已关联会话 · 具体片段未记录";
}
