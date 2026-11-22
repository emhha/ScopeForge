import { http } from "./api";

export type FlowNode = {
  id: string;
  type: "start" | "tool" | "transform" | "parallel" | "join" | "branch" | "end";
  operation?: string;
  branches?: string[];
  from?: string[];
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
  output_schema?: Record<string, unknown>;
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
  source?: string;
  agent_key?: string;
  parent_run_id?: string;
  input: unknown;
  result: string;
  error: string;
  created_at: string;
};
export type FlowEvent = {
  id: number;
  run_id: string;
  object: string;
  payload?: unknown;
  author: string;
  error?: string;
  created_at: string;
};
export type FlowTool = { name: string; description: string; input_schema?: Record<string, unknown> };
export type FlowRunSummary = { id: string; status: string; version: number; created_at: string };
export type FlowVersion = {
  id: number;
  flow_id: number;
  revision: number;
  definition: FlowDefinition;
  created_at: string;
};
export const flowAPI = {
  versions: (id: number) => http<FlowVersion[]>(`/flows/${id}/versions`),
  unpublish: (id: number) => http(`/flows/${id}/unpublish`, { method: "POST" }),
  rollback: (id: number, version: number) =>
    http<SavedFlow>(`/flows/${id}/rollback`, { method: "POST", body: JSON.stringify({ version_id: version }) }),
  validate: (definition: FlowDefinition) =>
    http<{ valid: boolean; diagnostics: string[] }>("/flows/validate", {
      method: "POST",
      body: JSON.stringify({ definition }),
    }),
  artifacts: (id: string) =>
    http<{ id: string; media_type: string; size: number; complete: boolean }[]>(`/flow-runs/${id}/artifacts`),
  allRuns: () => http<FlowRun[]>("/flow-runs"),
  inline: (definition: FlowDefinition, input: unknown) =>
    http<{ id: string }>("/flow-runs", { method: "POST", body: JSON.stringify({ definition, input }) }),
  saveRun: (id: string, name: string) =>
    http<SavedFlow>(`/flow-runs/${id}/save-as`, { method: "POST", body: JSON.stringify({ name }) }),
  runs: (id: number) => http<FlowRunSummary[]>(`/flows/${id}/runs`),
  catalog: () => http<{ tools: FlowTool[]; example: FlowDefinition; operations: string[] }>("/flows/catalog"),
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
