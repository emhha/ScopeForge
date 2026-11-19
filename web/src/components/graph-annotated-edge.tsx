"use client";

import { BaseEdge, type Edge, EdgeLabelRenderer, type EdgeProps, getBezierPath } from "@xyflow/react";

export type AnnotatedGraphEdge = Edge<{ annotation: string; relation: string; color: string }, "annotated">;

// Keep annotations on the line, bounded in size; full contribution details live
// in the existing node drawer rather than a second page-sized session list.
export function GraphAnnotatedEdge(props: EdgeProps<AnnotatedGraphEdge>) {
  const [path, x, y] = getBezierPath(props);
  const { data } = props;
  return (
    <>
      <BaseEdge id={props.id} path={path} markerEnd={props.markerEnd} style={props.style} />
      {data && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan pointer-events-auto absolute w-[200px] rounded-md border bg-card/95 px-2 py-1 text-center text-[10px] leading-snug shadow-sm"
            style={{ transform: `translate(-50%, -50%) translate(${x}px, ${y}px)` }}
            title={`${data.relation} · ${data.annotation}`}
          >
            <div className="font-medium" style={{ color: data.color }}>
              {data.relation}
            </div>
            <div className="line-clamp-2 break-words text-muted-foreground">{data.annotation}</div>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
