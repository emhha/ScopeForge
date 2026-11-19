"use client";

import { useCallback } from "react";

import { ExplorationGraph } from "@/components/exploration-graph";
import { Button } from "@/components/ui/button";
import { findingEdgeSessionLabel } from "@/lib/finding-provenance";
import type { Edge } from "@/lib/types";

import { NodeSourceSessions, type useFindingLineage } from "./provenance";

// Session names belong on the causal lines; full contributions stay in the drawer.
export function FindingLineageView({ lineage }: { lineage: ReturnType<typeof useFindingLineage> }) {
  const { data, error, retry } = lineage;
  const getEdgeAnnotation = useCallback(
    (edges: Edge[]) => findingEdgeSessionLabel(edges, data?.sessions ?? []),
    [data?.sessions],
  );
  if (error) {
    return (
      <div role="alert" className="flex flex-wrap items-center gap-2 text-destructive text-sm">
        无法加载发现链路：{error}
        <Button size="sm" variant="outline" onClick={retry}>
          重试
        </Button>
      </div>
    );
  }
  if (!data)
    return (
      <p role="status" className="p-6 text-muted-foreground text-sm">
        正在加载发现链路…
      </p>
    );
  if (!data.nodes.length) {
    return (
      <p className="p-6 text-muted-foreground text-sm">无链路可展示（该发现未关联探索节点，或所属任务已删除）。</p>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <p className="text-muted-foreground text-xs">
        展开原始意图与事实，连线标注来源与执行会话。点击节点只看该节点的会话；“历史关联”不代表已确认实际提交者。
      </p>
      <ExplorationGraph
        nodes={data.nodes}
        edges={data.edges}
        className="h-[68vh]"
        collapseDigests={false}
        getEdgeAnnotation={getEdgeAnnotation}
        renderNodeDetails={(node) => <NodeSourceSessions node={node} sessions={data.sessions} />}
      />
    </div>
  );
}
