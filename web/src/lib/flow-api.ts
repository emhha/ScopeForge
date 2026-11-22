import { http } from "./api";

export type FlowNode = {
  id: string;
  type: "start" | "tool" | "branch" | "end";
  position?: { x: number; y: number };
  tool?: string;
  arguments?: Record<string, unknown>;
  next?: string;
  condition?: { ref: string; equals: unknown; then: string; else: string };
  output?: unknown;
};
export type FlowDefinition = {
  name: string;
  description: string;
  version: number;
  input_schema: Record<string, unknown>;
  entry: string;
  nodes: FlowNode[];
};
export type SavedFlow = {
  id: number;
  name: string;
  definition: FlowDefinition;
  published?: FlowDefinition | null;
  revision: number;
  agents: string[];
  updated_at: string;
};
export type FlowRun = {
  id: string;
  flow_id: number;
  definition: FlowDefinition;
  status: "running" | "completed" | "failed" | "cancelled" | "interrupted";
  input: unknown;
  result: string;
  error: string;
  created_at: string;
};
export type FlowEvent = {
  id: number;
  run_id: string;
  object: string;
  author: string;
  error?: string;
  created_at: string;
};
export type FlowTool = { name: string; description: string; input_schema?: Record<string, unknown> };
export type FlowRunSummary = { id: string; status: string; version: number; created_at: string };
export const flowAPI = {
  runs: (id: number) => http<FlowRunSummary[]>(`/flows/${id}/runs`),
  catalog: () => http<{ tools: FlowTool[]; example: FlowDefinition }>("/flows/catalog"),
  list: () => http<SavedFlow[]>("/flows"),
  save: (definition: FlowDefinition, saved?: SavedFlow | null) =>
    http<SavedFlow>(saved ? `/flows/${saved.id}` : "/flows", {
      method: saved ? "PUT" : "POST",
      body: JSON.stringify({ definition, revision: saved?.revision ?? 0 }),
    }),
  publish: (saved: SavedFlow, agents: string[]) =>
    http<SavedFlow>(`/flows/${saved.id}/publish`, {
      method: "POST",
      body: JSON.stringify({ revision: saved.revision, agents }),
    }),
  run: (id: number, input: unknown, taskId?: number) =>
    http<{ id: string }>(`/flows/${id}/run`, {
      method: "POST",
      body: JSON.stringify({ input, task_id: taskId || 0 }),
    }),
  result: (id: string) => http<FlowRun>(`/flow-runs/${id}`),
  cancel: (id: string) => http(`/flow-runs/${id}/cancel`, { method: "POST" }),
};
