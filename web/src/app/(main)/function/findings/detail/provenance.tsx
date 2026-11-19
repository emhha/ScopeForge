"use client";

import * as React from "react";

import { ArrowUpRightIcon } from "lucide-react";
import { Link } from "react-router";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import { contributionLabels, sourcePrecisionLabel, sourceSessionHref } from "@/lib/finding-provenance";
import type { FindingLineage, FindingSourceSession, TaskNode } from "@/lib/types";

export function useFindingLineage(findingId: string) {
  const [result, setResult] = React.useState<{ id: string; data?: FindingLineage; error?: string }>();
  const [revision, setRevision] = React.useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: revision explicitly retries a failed request.
  React.useEffect(() => {
    if (!findingId) return;
    let alive = true;
    setResult({ id: findingId });
    api
      .findingLineage(findingId)
      .then((data) => {
        if (alive)
          setResult({
            id: findingId,
            data: { nodes: data.nodes ?? [], edges: data.edges ?? [], sessions: data.sessions ?? [] },
          });
      })
      .catch((e: unknown) => {
        if (alive) setResult({ id: findingId, error: e instanceof Error ? e.message : "无法加载发现溯源" });
      });
    return () => {
      alive = false;
    };
  }, [findingId, revision]);
  const current = result?.id === findingId ? result : undefined;
  return { data: current?.data, error: current?.error, retry: () => setRevision((v) => v + 1) };
}

export function NodeSourceSessions({
  node,
  sessions,
}: {
  node: Pick<TaskNode, "id" | "type">;
  sessions: FindingSourceSession[];
}) {
  return (
    <section className="border-t pt-3">
      <h4 className="mb-2 font-medium text-muted-foreground text-xs">此节点的来源与执行会话</h4>
      <SourceSessionList sessions={sessions} nodeId={node.id} />
    </section>
  );
}

export function SourceSessionList({ sessions, nodeId }: { sessions: FindingSourceSession[]; nodeId?: string }) {
  const relevant = sessions
    .map((s) => ({ ...s, contributions: s.contributions.filter((c) => !nodeId || c.node_id === nodeId) }))
    .filter((s) => s.contributions.length > 0);
  if (!relevant.length) return <p className="text-muted-foreground text-xs">来源会话未记录，无法可靠还原。</p>;
  return (
    <div className="flex flex-col gap-3">
      {relevant.map((s) => (
        <div key={`${s.task_id}:${s.session}`} className="rounded-md border p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="min-w-0">
              <p className="break-words font-medium text-sm">{s.title || s.session}</p>
              <p className="text-muted-foreground text-xs">
                任务 #{s.task_id} · {s.session}
              </p>
            </div>
            <Button asChild size="sm" variant="outline">
              <Link to={sourceSessionHref(s)}>
                打开会话
                <ArrowUpRightIcon className="size-3" />
              </Link>
            </Button>
          </div>
          <ul className="mt-2 flex flex-col gap-2">
            {s.contributions.map((c) => (
              <li key={`${c.node_id}:${c.relation}:${c.seq ?? c.precision}`} className="text-xs">
                <div className="flex items-start gap-2">
                  <Badge variant="secondary" className="shrink-0">
                    {contributionLabels[c.relation]}
                  </Badge>
                  <span className="min-w-0 break-words leading-relaxed">
                    {c.summary || `节点 #${c.node_id}`}
                    <span className="ml-1 text-muted-foreground">#{c.node_id}</span>
                  </span>
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-muted-foreground">
                  <span>{sourcePrecisionLabel(c)}</span>
                  {c.precision === "event" && !!c.seq && (
                    <Link className="text-primary underline underline-offset-4" to={sourceSessionHref(s, c)}>
                      查看相关片段
                    </Link>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  );
}
