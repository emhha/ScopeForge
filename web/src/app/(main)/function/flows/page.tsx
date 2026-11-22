"use client";

import * as React from "react";
import {
  ReactFlow,
  Background,
  Controls,
  Handle,
  Position,
  type Connection,
  type NodeProps,
  type NodeChange,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { Plus, Save, Play, Square, GitBranch, Upload, CheckCircle2, Loader2, Code2, Workflow } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { sseUrl } from "@/lib/api";
import { reconcileFlowRun } from "@/lib/flow-state";
import {
  flowAPI,
  type FlowDefinition,
  type FlowNode,
  type FlowTool,
  type SavedFlow,
  type FlowRun,
  type FlowEvent,
  type FlowRunSummary,
} from "@/lib/flow-api";

const labels = { start: "开始", tool: "工具调用", branch: "条件分支", end: "结束" };
function NodeCard({ data, selected }: NodeProps) {
  const type = data.kind as FlowNode["type"];
  const status = String(data.status ?? "");
  return (
    <div
      className={`min-w-40 rounded-xl border-2 bg-card px-4 py-3 shadow-sm ${selected ? "border-primary" : status === "failed" ? "border-red-500" : status === "running" ? "border-blue-500" : status === "completed" ? "border-emerald-500" : "border-border"}`}
    >
      {type !== "start" && <Handle type="target" position={Position.Left} />}
      <div className="mb-1 flex items-center gap-2 text-[11px] text-muted-foreground">
        <GitBranch className="size-3" />
        {labels[type]}
        {status === "running" && <Loader2 className="size-3 animate-spin" />}
        {status === "completed" && <CheckCircle2 className="size-3 text-emerald-600" />}
      </div>
      <div className="text-sm font-medium">{String(data.label)}</div>
      {type === "tool" && (
        <div className="mt-1 font-mono text-xs text-muted-foreground">{String(data.tool ?? "未选择工具")}</div>
      )}
      {type === "branch" ? (
        <>
          <Handle id="then" type="source" position={Position.Right} style={{ top: "35%" }} />
          <Handle id="else" type="source" position={Position.Right} style={{ top: "75%" }} />
        </>
      ) : (
        type !== "end" && <Handle type="source" position={Position.Right} />
      )}
    </div>
  );
}
const nodeTypes = { flow: NodeCard };
const pretty = (value: unknown) => JSON.stringify(value, null, 2);
function JsonField({
  label,
  value,
  onCommit,
  onError,
}: {
  label: string;
  value: unknown;
  onCommit: (value: unknown) => void;
  onError: (message: string) => void;
}) {
  const serialized = pretty(value ?? {});
  const [text, setText] = React.useState(serialized);
  React.useEffect(() => setText(serialized), [serialized]);
  return (
    <label className="block space-y-1.5 text-xs">
      <span className="font-medium">{label}</span>
      <Textarea
        aria-label={label}
        className="min-h-28 font-mono text-xs"
        value={text}
        onChange={(event) => setText(event.target.value)}
        onBlur={() => {
          try {
            onCommit(JSON.parse(text));
          } catch {
            onError(`${label} 不是有效 JSON`);
          }
        }}
      />
    </label>
  );
}

export default function FlowsPage() {
  const [catalog, setCatalog] = React.useState<{ tools: FlowTool[]; example: FlowDefinition }>();
  const [rows, setRows] = React.useState<SavedFlow[]>([]);
  const [saved, setSaved] = React.useState<SavedFlow | null>(null);
  const [draft, setDraft] = React.useState<FlowDefinition>();
  const [selected, setSelected] = React.useState("inspect");
  const [mode, setMode] = React.useState<"graph" | "json">("graph");
  const [json, setJSON] = React.useState("");
  const [input, setInput] = React.useState('{\n  "target": "https://example.test"\n}');
  const [taskID, setTaskID] = React.useState("");
  const [bindings, setBindings] = React.useState("auto,mainagent,worker");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [notice, setNotice] = React.useState("");
  const [run, setRun] = React.useState<FlowRun | null>(null);
  const [runID, setRunID] = React.useState("");
  const [events, setEvents] = React.useState<FlowEvent[]>([]);
  const [runs, setRuns] = React.useState<FlowRunSummary[]>([]);
  const [measurements, setMeasurements] = React.useState<Record<string, { width: number; height: number }>>({});
  const [connection, setConnection] = React.useState("");
  const dirty = !!draft && (!saved || pretty(draft) !== pretty(saved.definition));
  const active = draft?.nodes.find((node) => node.id === selected);

  React.useEffect(() => {
    let alive = true;
    Promise.all([flowAPI.catalog(), flowAPI.list()])
      .then(([c, items]) => {
        if (!alive) return;
        setCatalog(c);
        setRows(items);
        const initial = items[0];
        setSaved(initial ?? null);
        setDraft(initial?.definition ?? c.example);
        if (initial?.agents.length) setBindings(initial.agents.join(","));
      })
      .catch((err) => {
        if (alive) setError(String(err.message ?? err));
      });
    return () => {
      alive = false;
    };
  }, []);

  React.useEffect(() => {
    setRuns([]);
    setRunID("");
    setRun(null);
    setEvents([]);
    if (!saved?.id) return;
    let alive = true;
    flowAPI
      .runs(saved.id)
      .then((items) => {
        if (!alive) return;
        setRuns(items);
        if (items[0]) setRunID((current) => current || items[0].id);
      })
      .catch((err) => {
        if (alive) setError(err.message);
      });
    return () => {
      alive = false;
    };
  }, [saved?.id]);
  React.useEffect(() => {
    if (!runID) return;
    let alive = true;
    const source = new EventSource(sseUrl(`/api/flow-runs/${runID}/events?since=0`));
    source.onopen = () => {
      if (alive) setConnection("实时连接正常");
    };
    source.onerror = () => {
      if (alive) setConnection("连接中断，正在自动补读并重连");
    };
    source.onmessage = (event) => {
      if (!alive) return;
      try {
        const item = JSON.parse(event.data) as FlowEvent;
        setEvents((previous) => (previous.some((old) => old.id === item.id) ? previous : [...previous, item]));
      } catch {
        /* Ignore malformed frames. */
      }
    };
    source.addEventListener("done", (event) => {
      if (!alive) return;
      const finished = JSON.parse((event as MessageEvent).data) as FlowRun;
      setRun(finished);
      void flowAPI.runs(finished.flow_id).then((items) => {
        if (alive) setRuns(items);
      });
      setConnection("运行结束，记录已保存");
      source.close();
    });
    flowAPI
      .result(runID)
      .then((value) => {
        if (alive) setRun((current) => reconcileFlowRun(current, value));
      })
      .catch((err) => {
        if (alive) setError(err.message);
      });
    return () => {
      alive = false;
      source.close();
    };
  }, [runID]);

  const statuses = React.useMemo(() => {
    const states: Record<string, string> = {};
    for (const item of events) {
      const id = item.author.replace(/^(node_|tool_|collect_)/, "");
      if (item.object === "graph.node.error" || item.error) states[id] = "failed";
      else if (item.object === "graph.node.start") states[id] = "running";
      else if (item.object === "graph.node.complete") states[id] = "completed";
    }
    return states;
  }, [events]);
  const nodes = React.useMemo(
    () =>
      draft?.nodes.map((node, index) => ({
        id: node.id,
        measured: measurements[node.id],
        type: "flow",
        selected: node.id === selected,
        position: node.position ?? { x: Math.min(index, 3) * 240 + 40, y: node.id === "rejected" ? 250 : 70 },
        data: { label: node.id, kind: node.type, tool: node.tool, status: statuses[node.id] },
      })) ?? [],
    [draft, selected, statuses, measurements],
  );
  const edges = React.useMemo(
    () =>
      draft?.nodes.flatMap((node) => {
        const targets =
          node.type === "branch"
            ? [
                { target: node.condition?.then, handle: "then", label: "成立" },
                { target: node.condition?.else, handle: "else", label: "不成立" },
              ]
            : [{ target: node.next, handle: undefined, label: "" }];
        return targets
          .filter((item) => item.target)
          .map((item) => ({
            id: `${node.id}-${item.handle ?? "next"}`,
            source: node.id,
            target: item.target!,
            sourceHandle: item.handle,
            label: item.label,
            animated: statuses[node.id] === "running",
          }));
      }) ?? [],
    [draft, statuses],
  );

  function updateNode(patch: Partial<FlowNode>) {
    setDraft((current) =>
      current
        ? { ...current, nodes: current.nodes.map((node) => (node.id === selected ? { ...node, ...patch } : node)) }
        : current,
    );
  }
  function connect(connection: Connection) {
    setDraft((current) =>
      current
        ? {
            ...current,
            nodes: current.nodes.map((node) => {
              if (node.id !== connection.source) return node;
              if (node.type === "branch")
                return {
                  ...node,
                  condition: {
                    ref: "input.target",
                    equals: "",
                    then: "",
                    else: "",
                    ...node.condition,
                    [connection.sourceHandle === "else" ? "else" : "then"]: connection.target,
                  },
                };
              return { ...node, next: connection.target };
            }),
          }
        : current,
    );
  }
  function nodeChanges(changes: NodeChange[]) {
    const sizes = changes.filter((change) => change.type === "dimensions");
    if (sizes.length)
      setMeasurements((previous) => {
        let next = previous;
        for (const change of sizes) {
          if (change.type !== "dimensions" || !change.dimensions) continue;
          const old = previous[change.id];
          if (old?.width === change.dimensions.width && old?.height === change.dimensions.height) continue;
          next = { ...next, [change.id]: change.dimensions };
        }
        return next;
      });
    if (!changes.some((change) => change.type === "position" || change.type === "remove")) return;

    setDraft((current) =>
      current
        ? {
            ...current,
            nodes: current.nodes.flatMap((node) => {
              if (changes.some((change) => change.type === "remove" && change.id === node.id)) return [];
              const move = changes.find((change) => change.type === "position" && change.id === node.id);
              return [{ ...node, ...(move?.type === "position" && move.position ? { position: move.position } : {}) }];
            }),
          }
        : current,
    );
  }
  function addNode(type: FlowNode["type"]) {
    if (!draft) return;
    const id = `${type}_${Date.now().toString(36)}`;
    const node: FlowNode = {
      id,
      type,
      position: { x: 240, y: 300 },
      ...(type === "tool" ? { tool: "inspect", arguments: { target: { $ref: "input.target" } } } : {}),
      ...(type === "branch" ? { condition: { ref: "steps.inspect.text", equals: "allowed", then: "", else: "" } } : {}),
      ...(type === "end" ? { output: {} } : {}),
    };
    setDraft({ ...draft, nodes: [...draft.nodes, node] });
    setSelected(id);
  }
  function currentDefinition() {
    const value = mode === "json" ? JSON.parse(json) : draft;
    if (!value || !Array.isArray(value.nodes) || !value.input_schema) throw Error("流程必须包含 nodes 和 input_schema");
    return value as FlowDefinition;
  }
  async function persist() {
    const value = await flowAPI.save(currentDefinition(), saved);
    setSaved(value);
    setDraft(value.definition);
    setJSON(pretty(value.definition));
    setRows((previous) => [value, ...previous.filter((item) => item.id !== value.id)]);
    return value;
  }
  async function action(kind: "save" | "publish" | "run") {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      // Save/validate before executing. In-flight runs keep their own frozen version.
      const value = await persist();
      if (kind === "publish") {
        const published = await flowAPI.publish(
          value,
          bindings
            .split(",")
            .map((key) => key.trim())
            .filter(Boolean),
        );
        setSaved(published);
        setRows((previous) => previous.map((item) => (item.id === published.id ? published : item)));
        setNotice(`已发布 flow_${value.name}，绑定 Agent 的下一次运行可调用`);
      } else if (kind === "run") {
        const inputValue = JSON.parse(input);
        const started = await flowAPI.run(value.id, inputValue, taskID ? Number(taskID) : undefined);
        setEvents([]);
        setRun(null);
        setRunID(started.id);
        setRuns((previous) => [
          { id: started.id, status: "running", version: value.revision, created_at: new Date().toISOString() },
          ...previous,
        ]);
        setNotice("试运行已开始");
      } else setNotice(`已保存版本 ${value.revision}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }
  function openFlow(value?: SavedFlow) {
    if (dirty && !window.confirm("当前更改尚未保存，是否切换？")) return;
    const def =
      value?.definition ??
      (catalog ? { ...structuredClone(catalog.example), name: `assessment_${Date.now().toString(36)}` } : undefined);
    setSaved(value ?? null);
    setDraft(def);
    setSelected(def?.entry ?? "");
    setMode("graph");
    setError("");
    setNotice("");
    setBindings(value?.agents.length ? value.agents.join(",") : "auto,mainagent,worker");
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 p-4 lg:p-6" data-testid="flows-page">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <Workflow className="size-5" />
            工作流 Flow
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">编排可复用的执行步骤，发布后供 Agent 作为工具调用。</p>
        </div>
        <Button onClick={() => openFlow()} disabled={!catalog}>
          <Plus className="size-4" />
          新建流程
        </Button>
      </header>
      {error && (
        <div role="alert" className="rounded-lg border border-red-500/30 bg-red-500/5 p-3 text-sm text-red-600">
          {error}
        </div>
      )}
      {notice && (
        <div role="status" className="rounded-lg border bg-muted/40 p-3 text-sm">
          {notice}
        </div>
      )}
      {!draft ? (
        <p className="p-8 text-sm text-muted-foreground">正在加载流程…</p>
      ) : (
        <div className="grid min-h-0 flex-1 gap-4 xl:grid-cols-[210px_minmax(0,1fr)]">
          <aside className="space-y-3 rounded-xl border bg-card p-3">
            <div className="text-xs font-medium text-muted-foreground">已保存流程</div>
            {rows.length === 0 && <p className="text-xs text-muted-foreground">示例已载入，可直接点击试运行。</p>}
            {rows.map((item) => (
              <button
                key={item.id}
                onClick={() => openFlow(item)}
                className={`block w-full rounded-lg border p-3 text-left text-sm ${saved?.id === item.id ? "border-primary bg-primary/5" : "border-transparent hover:bg-muted"}`}
              >
                <span className="block truncate font-medium">{item.name}</span>
                <span className="text-xs text-muted-foreground">
                  草稿 v{item.revision} · {item.published ? `已发布 v${item.published.version}` : "未发布"}
                </span>
              </button>
            ))}
            <div className="border-t pt-3 text-xs leading-relaxed text-muted-foreground">
              示例 inspect 只判断字符串，不发网络请求。HTTPS 输入走接受分支，HTTP 输入走拒绝分支。
            </div>
          </aside>
          <main className="min-w-0 space-y-4">
            <section className="rounded-xl border bg-card p-4">
              <div className="flex flex-wrap gap-3">
                <label className="min-w-48 flex-1 space-y-1 text-xs">
                  流程名称
                  <Input
                    aria-label="流程名称"
                    value={draft.name}
                    onChange={(event) => setDraft({ ...draft, name: event.target.value })}
                  />
                </label>
                <label className="min-w-64 flex-[2] space-y-1 text-xs">
                  说明
                  <Input
                    aria-label="流程说明"
                    value={draft.description}
                    onChange={(event) => setDraft({ ...draft, description: event.target.value })}
                  />
                </label>
                <div className="flex items-end gap-2">
                  <Button variant="outline" onClick={() => void action("save")} disabled={busy}>
                    <Save className="size-4" />
                    保存
                  </Button>
                  <Button variant="outline" onClick={() => void action("publish")} disabled={busy}>
                    <Upload className="size-4" />
                    发布为工具
                  </Button>
                </div>
              </div>
              <div className="mt-3 flex flex-wrap items-center gap-3 text-xs">
                <label className="flex items-center gap-2">
                  绑定 Agent
                  <Input
                    aria-label="绑定 Agent"
                    className="h-7 w-64 font-mono text-xs"
                    value={bindings}
                    onChange={(event) => setBindings(event.target.value)}
                  />
                </label>
                <span className="text-muted-foreground">
                  工具名 flow_{draft.name} · {dirty ? "有未保存更改" : `草稿 v${saved?.revision ?? 1}`}
                </span>
              </div>
            </section>
            <section className="overflow-hidden rounded-xl border bg-card">
              <div className="flex flex-wrap items-center justify-between gap-2 border-b p-3">
                <div className="flex gap-1">
                  {(["graph", "json"] as const).map((value) => (
                    <Button
                      key={value}
                      size="sm"
                      variant={mode === value ? "secondary" : "ghost"}
                      onClick={() => {
                        try {
                          if (value === "graph" && mode === "json") setDraft(currentDefinition());
                          if (value === "json") setJSON(pretty(draft));
                          setMode(value);
                        } catch (err) {
                          setError(String(err));
                        }
                      }}
                    >
                      {value === "graph" ? (
                        "可视化编排"
                      ) : (
                        <>
                          <Code2 className="size-3" />
                          JSON
                        </>
                      )}
                    </Button>
                  ))}
                </div>
                {mode === "graph" && (
                  <div className="flex gap-1">
                    {(["tool", "branch", "end"] as const).map((type) => (
                      <Button key={type} size="sm" variant="outline" onClick={() => addNode(type)}>
                        <Plus className="size-3" />
                        {labels[type]}
                      </Button>
                    ))}
                  </div>
                )}
              </div>
              {mode === "json" ? (
                <Textarea
                  aria-label="流程 JSON"
                  value={json}
                  onChange={(event) => setJSON(event.target.value)}
                  className="min-h-[480px] rounded-none border-0 font-mono text-xs"
                />
              ) : (
                <div className="grid lg:grid-cols-[minmax(0,1fr)_280px]">
                  <div className="h-[480px] min-w-0 bg-muted/20">
                    <ReactFlow
                      nodes={nodes}
                      edges={edges}
                      nodeTypes={nodeTypes}
                      onNodesChange={nodeChanges}
                      onNodeClick={(_, node) => setSelected(node.id)}
                      onConnect={connect}
                      onEdgesDelete={(deleted) => {
                        setDraft((current) =>
                          current
                            ? {
                                ...current,
                                nodes: current.nodes.map((node) => {
                                  const edge = deleted.find((item) => item.source === node.id);
                                  if (!edge) return node;
                                  if (node.type === "branch" && node.condition)
                                    return {
                                      ...node,
                                      condition: {
                                        ...node.condition,
                                        [edge.sourceHandle === "else" ? "else" : "then"]: "",
                                      },
                                    };
                                  return { ...node, next: "" };
                                }),
                              }
                            : current,
                        );
                      }}
                      fitView
                      minZoom={0.3}
                    >
                      <Background />
                      <Controls />
                    </ReactFlow>
                  </div>
                  <div className="space-y-4 border-l p-4">
                    {active ? (
                      <>
                        <div>
                          <div className="text-xs text-muted-foreground">{labels[active.type]}</div>
                          <h2 className="font-mono text-sm font-semibold">{active.id}</h2>
                        </div>
                        {active.type === "tool" && (
                          <>
                            <label className="block space-y-1 text-xs">
                              工具
                              <select
                                aria-label="节点工具"
                                className="h-9 w-full rounded-md border bg-background px-2"
                                value={active.tool ?? ""}
                                onChange={(event) => updateNode({ tool: event.target.value })}
                              >
                                {catalog?.tools.map((item) => (
                                  <option key={item.name} value={item.name}>
                                    {item.name}
                                  </option>
                                ))}
                              </select>
                            </label>
                            <JsonField
                              key={`${active.id}-args`}
                              label="工具参数 JSON"
                              value={active.arguments}
                              onCommit={(value) => updateNode({ arguments: value as Record<string, unknown> })}
                              onError={setError}
                            />
                            <p className="text-xs text-muted-foreground">引用格式：{'{"$ref":"input.target"}'}</p>
                          </>
                        )}
                        {active.type === "branch" && (
                          <>
                            <label className="block space-y-1 text-xs">
                              判断字段
                              <Input
                                aria-label="判断字段"
                                value={active.condition?.ref ?? ""}
                                onChange={(event) =>
                                  updateNode({
                                    condition: {
                                      equals: "",
                                      then: "",
                                      else: "",
                                      ...active.condition,
                                      ref: event.target.value,
                                    },
                                  })
                                }
                              />
                            </label>
                            <JsonField
                              key={`${active.id}-equals`}
                              label="等于（JSON 值）"
                              value={active.condition?.equals}
                              onCommit={(value) =>
                                updateNode({
                                  condition: { ref: "", then: "", else: "", ...active.condition, equals: value },
                                })
                              }
                              onError={setError}
                            />
                            <p className="text-xs text-muted-foreground">上方连接点为成立，下方为不成立。</p>
                          </>
                        )}
                        {active.type === "end" && (
                          <JsonField
                            key={active.id}
                            label="输出 JSON"
                            value={active.output}
                            onCommit={(value) => updateNode({ output: value })}
                            onError={setError}
                          />
                        )}
                        {active.type === "start" && (
                          <JsonField
                            key={active.id}
                            label="输入 Schema"
                            value={draft.input_schema}
                            onCommit={(value) => setDraft({ ...draft, input_schema: value as Record<string, unknown> })}
                            onError={setError}
                          />
                        )}
                        {active.type !== "start" && (
                          <Button
                            size="sm"
                            variant="outline"
                            onClick={() => {
                              setDraft({ ...draft, nodes: draft.nodes.filter((node) => node.id !== active.id) });
                              setSelected(draft.entry);
                            }}
                          >
                            删除节点
                          </Button>
                        )}
                      </>
                    ) : (
                      <p className="text-xs text-muted-foreground">点击节点编辑参数，拖动连接点编排步骤。</p>
                    )}
                  </div>
                </div>
              )}
            </section>
            <section className="grid gap-4 rounded-xl border bg-card p-4 lg:grid-cols-[300px_minmax(0,1fr)]">
              <div className="space-y-3">
                <h2 className="text-sm font-semibold">试运行</h2>
                {runs.length > 0 && (
                  <label className="block space-y-1 text-xs">
                    运行记录
                    <select
                      aria-label="运行记录"
                      className="h-9 w-full rounded-md border bg-background px-2"
                      value={runID}
                      onChange={(event) => {
                        setEvents([]);
                        setRun(null);
                        setRunID(event.target.value);
                      }}
                    >
                      {runs.map((item) => (
                        <option key={item.id} value={item.id}>
                          v{item.version} · {item.status} · {new Date(item.created_at).toLocaleTimeString()}
                        </option>
                      ))}
                    </select>
                  </label>
                )}

                <Textarea
                  aria-label="试运行输入"
                  className="min-h-32 font-mono text-xs"
                  value={input}
                  onChange={(event) => setInput(event.target.value)}
                />
                <Input
                  aria-label="关联任务 ID"
                  placeholder="关联任务 ID（可选，用于资产范围）"
                  type="number"
                  min={1}
                  value={taskID}
                  onChange={(event) => setTaskID(event.target.value)}
                />
                <div className="flex gap-2">
                  <Button
                    onClick={() => void action("run")}
                    disabled={busy || run?.status === "running" || (!!runID && !run)}
                  >
                    <Play className="size-4" />
                    试运行
                  </Button>
                  {run?.status === "running" && (
                    <Button
                      variant="outline"
                      onClick={() => void flowAPI.cancel(run.id).catch((err) => setError(err.message))}
                    >
                      <Square className="size-3" />
                      停止
                    </Button>
                  )}
                </div>
                <p className="text-xs text-muted-foreground">
                  试运行会保存并校验当前草稿。运行最长 5 分钟；模型和网络工具使用现有系统配置。
                </p>
              </div>
              <div className="min-w-0 space-y-2">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <h2 className="text-sm font-semibold">
                    节点执行过程{" "}
                    {run && <span className="ml-2 font-mono text-xs text-muted-foreground">{run.status}</span>}
                  </h2>
                  <span className="text-xs text-muted-foreground">{connection}</span>
                </div>
                <div
                  aria-label="节点执行日志"
                  className="h-44 overflow-auto rounded-lg border bg-muted/30 p-2 font-mono text-xs"
                >
                  {events.length ? (
                    events.map((item) => (
                      <div key={item.id} className="border-b border-border/40 py-1">
                        <span className="text-muted-foreground">{new Date(item.created_at).toLocaleTimeString()} </span>
                        {item.author} · {item.object}
                        {item.error && <span className="text-red-600"> {item.error}</span>}
                      </div>
                    ))
                  ) : (
                    <p className="p-3 text-muted-foreground">运行后在这里实时查看节点开始、完成或失败。</p>
                  )}
                </div>
                {run?.error && (
                  <p role="alert" className="text-sm text-red-600">
                    {run.error}
                  </p>
                )}
                {run?.result && (
                  <pre
                    aria-label="运行结果"
                    className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-3 text-xs"
                  >
                    {(() => {
                      try {
                        return pretty(JSON.parse(run.result));
                      } catch {
                        return run.result;
                      }
                    })()}
                  </pre>
                )}
              </div>
            </section>
          </main>
        </div>
      )}
    </div>
  );
}
