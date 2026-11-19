// Sanitized regression topology from finding #2. No business payloads, API
// requests, login state, or production credentials are used by this fixture.
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router";

import { FindingLineageView } from "@/app/(main)/function/findings/detail/lineage";
import "@/app/globals.css";
import type { Edge, FindingLineage, FindingSourceSession, TaskNode } from "./types";

const node = (id: number, type: TaskNode["type"], state = "confirmed"): TaskNode => ({
  id: String(id),
  type,
  state,
  priority: 0,
  origin: "test-fixture",
  ts: "2026-01-01T00:00:00Z",
  payload: JSON.stringify({ summary: `${type} #${id}` }),
});
const edges: Edge[] = [
  ["5", "15", "derived_from"],
  ["15", "119", "yields"],
  ["119", "124", "derived_from"],
  ["124", "125", "yields"],
  ["124", "126", "yields"],
  ["125", "127", "derived_from"],
  ["126", "127", "derived_from"],
  ["127", "133", "yields"],
  ["125", "161", "derived_from"],
  ["126", "161", "derived_from"],
  ["133", "161", "derived_from"],
  ["161", "166", "yields"],
  ["125", "168", "derived_from"],
  ["126", "168", "derived_from"],
  ["166", "168", "derived_from"],
  ["168", "176", "yields"],
].map(([src, dst, rel]) => ({ src, dst, rel: rel as Edge["rel"] }));
const intentOutputs: Array<[number, number[]]> = [
  [15, [119]],
  [124, [125, 126]],
  [127, [133]],
  [161, [166]],
  [168, [176]],
];
const sessions: FindingSourceSession[] = intentOutputs.map(([id, outputs]) => ({
  task_id: "3",
  session: `intent:${id}`,
  title: `测试意图 #${id}`,
  role: "worker",
  contributions: [
    { node_id: String(id), relation: "executed", precision: "session", summary: `执行意图 #${id}` },
    ...outputs.map((output) => ({
      node_id: String(output),
      relation: "produced" as const,
      precision: "legacy" as const,
      summary: `历史产出关联 #${output}`,
    })),
  ],
}));
const data: FindingLineage = {
  nodes: [
    node(5, "fact", "origin"),
    node(15, "intent", "done"),
    node(119, "fact"),
    node(124, "intent", "done"),
    node(125, "fact"),
    node(126, "finding"),
    node(127, "intent", "done"),
    node(133, "fact"),
    node(161, "intent", "done"),
    node(166, "fact"),
    node(168, "intent", "done"),
    node(176, "finding"),
    node(413, "digest", "active"),
  ],
  edges: [
    ...edges,
    ...[15, 119, 124, 125, 127, 133, 161, 166, 168].map((id) => ({
      src: "413",
      dst: String(id),
      rel: "covers" as const,
    })),
  ],
  sessions,
};
const root = document.getElementById("root");
if (!root) throw new Error("Missing fixture root");
createRoot(root).render(
  <MemoryRouter>
    <FindingLineageView lineage={{ data, error: undefined, retry: () => undefined }} />
  </MemoryRouter>,
);
