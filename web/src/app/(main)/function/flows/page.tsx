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
import { reconcileFlowRun, renameFlowNode, removeFlowNodes } from "@/lib/flow-state";
import {
  flowAPI,
  type FlowDefinition,
  type FlowNode,
  type FlowTool,
  type SavedFlow,
  type FlowRun,
  type FlowEvent,
  type FlowRunSummary,
  type FlowVersion,
} from "@/lib/flow-api";

const labels = {
  start: "开始",
  tool: "工具调用",
  transform: "数据转换",
  parallel: "并行分叉",
  join: "等待汇合",
  branch: "条件分支",
  end: "结束",
};
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
      {type === "parallel" ? (
        <>
          <Handle id="new" type="source" position={Position.Right} />
          {((data.branches as string[]) ?? []).map((_, i, list) => (
            <Handle
              key={i}
              id={`branch-${i}`}
              type="source"
              position={Position.Right}
              style={{ top: `${20 + (i * 60) / Math.max(1, list.length - 1)}%` }}
            />
          ))}
        </>
      ) : type === "branch" ? (
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
function exampleInput(def: FlowDefinition) {
  const properties = (def.input_schema.properties ?? {}) as Record<string, { default?: unknown; type?: string }>;
  return pretty(
    Object.fromEntries(
      Object.entries(properties)
        .filter(
          ([key, schema]) =>
            schema.default !== undefined || ((def.input_schema.required as string[] | undefined) ?? []).includes(key),
        )
        .map(([key, schema]) => [
          key,
          schema.default ??
            (schema.type === "string"
              ? key === "domain"
                ? "example.com"
                : key === "target" || key === "url"
                  ? "https://example.test"
                  : ""
              : schema.type === "array"
                ? []
                : schema.type === "number" || schema.type === "integer"
                  ? 0
                  : schema.type === "boolean"
                    ? false
                    : {}),
        ]),
    ),
  );
}
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
  const serialized = pretty(value === undefined ? {} : value);
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
  const [catalog, setCatalog] = React.useState<{ tools: FlowTool[]; example: FlowDefinition; operations: string[] }>();
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
  const [showNative, setShowNative] = React.useState(false);
  const [toolQuery, setToolQuery] = React.useState("");
  const [versions, setVersions] = React.useState<FlowVersion[]>([]);
  const [artifacts, setArtifacts] = React.useState<
    { id: string; media_type: string; size: number; complete: boolean }[]
  >([]);
  const [allRuns, setAllRuns] = React.useState<FlowRun[]>([]);
  const [measurements, setMeasurements] = React.useState<Record<string, { width: number; height: number }>>({});
  const [connection, setConnection] = React.useState("");
  let dirty = !!draft && (!saved || pretty(draft) !== pretty(saved.definition));
  if (mode === "json") {
    try {
      dirty = !saved || pretty(JSON.parse(json)) !== pretty(saved.definition);
    } catch {
      dirty = true;
    }
  }
  const active = draft?.nodes.find((node) => node.id === selected);

  React.useEffect(() => {
    let alive = true;
    Promise.all([flowAPI.catalog(), flowAPI.list()])
      .then(([c, items]) => {
        if (!alive) return;
        setCatalog(c);
        setRows(items);
        const requested = Number(new URLSearchParams(window.location.search).get("id"));
        const initial = items.find((item) => item.id === requested) ?? items[0];
        setSaved(initial ?? null);
        setDraft(initial?.definition ?? c.example);
        setInput(exampleInput(initial?.definition ?? c.example));
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
    const reload = () => {
      void flowAPI
        .catalog()
        .then(setCatalog)
        .catch((err) => setError(err.message));
      void flowAPI
        .allRuns()
        .then(setAllRuns)
        .catch(() => {});
    };
    window.addEventListener("focus", reload);
    reload();
    return () => window.removeEventListener("focus", reload);
  }, []);
  React.useEffect(() => {
    setVersions([]);
    if (saved?.id)
      void flowAPI
        .versions(saved.id)
        .then(setVersions)
        .catch((err) => setError(err.message));
  }, [saved?.id, saved?.revision, saved?.published]);
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
    setArtifacts([]);
    if (!runID) return;
    let alive = true;
    void flowAPI
      .artifacts(runID)
      .then((items) => {
        if (alive) setArtifacts(items);
      })
      .catch(() => {});
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
      void flowAPI
        .artifacts(finished.id)
        .then(setArtifacts)
        .catch(() => {});
      if (finished.flow_id > 0)
        void flowAPI
          .runs(finished.flow_id)
          .then((items) => {
            if (alive) setRuns(items);
          })
          .catch((err) => {
            if (alive) setError(err.message);
          });
      void flowAPI
        .allRuns()
        .then(setAllRuns)
        .catch(() => {});
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
      if (item.run_id !== runID) continue;
      const payload = item.payload as { branch?: string } | undefined;
      const branchNode = payload?.branch?.match(/(?:^|\/)tool_([^/]+)/)?.[1];
      const id = branchNode ?? item.author.replace(/^(node_|tool_|collect_)/, "");
      if (item.object === "graph.node.error" || item.error) states[id] = "failed";
      else if (item.object === "graph.node.start") states[id] = "running";
      else if (item.object === "graph.node.complete") states[id] = "completed";
    }
    return states;
  }, [events, runID]);
  const visibleEvents = showNative
    ? events
    : events.filter((item) => {
        if (item.error) return true;
        if (!["graph.node.start", "graph.node.complete", "graph.node.error"].includes(item.object)) return false;
        if (item.author.startsWith("tool_")) return true;
        if (item.author.startsWith("node_")) {
          const node = draft?.nodes.find((node) => `node_${node.id}` === item.author);
          return node?.type !== "tool";
        }
        return false;
      });
  const nodes = React.useMemo(
    () =>
      draft?.nodes.map((node, index) => ({
        id: node.id,
        measured: measurements[node.id],
        deletable: node.type !== "start",
        type: "flow",
        selected: node.id === selected,
        position: node.position ?? { x: Math.min(index, 3) * 240 + 40, y: node.id === "rejected" ? 250 : 70 },
        data: { label: node.id, kind: node.type, tool: node.tool, branches: node.branches, status: statuses[node.id] },
      })) ?? [],
    [draft, selected, statuses, measurements],
  );
  const edges = React.useMemo(
    () =>
      draft?.nodes.flatMap((node) => {
        const targets =
          node.type === "parallel"
            ? (node.branches ?? []).map((target, i) => ({ target, handle: `branch-${i}`, label: "" }))
            : node.type === "branch"
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
              if (node.id === connection.target && node.type === "join")
                return { ...node, from: Array.from(new Set([...(node.from ?? []), connection.source])) };
              if (node.id !== connection.source) return node;
              if (node.type === "parallel") {
                const branches = [...(node.branches ?? [])];
                const index = Number(connection.sourceHandle?.replace("branch-", ""));
                if (connection.sourceHandle?.startsWith("branch-") && Number.isFinite(index))
                  branches[index] = connection.target;
                else if (!branches.includes(connection.target)) branches.push(connection.target);
                return { ...node, branches };
              }
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

    setDraft((current) => {
      if (!current) return current;
      const next = removeFlowNodes(
        current,
        changes.filter((change) => change.type === "remove").map((change) => change.id),
      );
      return {
        ...next,
        nodes: next.nodes.map((node) => {
          const move = changes.find((change) => change.type === "position" && change.id === node.id);
          return { ...node, ...(move?.type === "position" && move.position ? { position: move.position } : {}) };
        }),
      };
    });
  }
  function addNode(type: FlowNode["type"]) {
    if (!draft) return;
    const id = `${type}_${Date.now().toString(36)}`;
    const node: FlowNode = {
      id,
      type,
      position: { x: 240, y: 300 },
      ...(type === "tool" ? { tool: "inspect", arguments: { target: { $ref: "input.target" } } } : {}),
      ...(type === "transform" ? { operation: "json.parse", arguments: { text: "" } } : {}),
      ...(type === "parallel" ? { branches: [] } : {}),
      ...(type === "join" ? { from: [] } : {}),
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
    const definition = currentDefinition();
    if (saved && pretty(definition) === pretty(saved.definition)) {
      setJSON(pretty(saved.definition));
      return saved;
    }
    const value = await flowAPI.save(definition, saved);
    setSaved(value);
    setDraft(value.definition);
    setJSON(pretty(value.definition));
    setRows((previous) => [value, ...previous.filter((item) => item.id !== value.id)]);
    return value;
  }
  async function action(kind: "save" | "publish" | "run" | "inline" | "validate") {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      // Save/validate before executing. In-flight runs keep their own frozen version.
      if (kind === "validate") {
        const checked = await flowAPI.validate(currentDefinition());
        if (!checked.valid) throw Error(checked.diagnostics.join("\n"));
        setNotice("校验通过：原生 Graph 编译及工具依赖有效");
        return;
      }
      if (kind === "inline") {
        const started = await flowAPI.inline(currentDefinition(), JSON.parse(input));
        setEvents([]);
        setRun(null);
        setRunID(started.id);
        setNotice("临时运行已开始，不保存草稿、不注册工具；过程会落盘");
        return;
      }
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
    if (def) setInput(exampleInput(def));
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
              工具目录包含已启用的自定义工具和 MCP。命令环境声明可用 argv 输入；通用 workflow_command 支持 stdin
              和工件。
            </div>
            <div className="border-t pt-3 space-y-2">
              <div className="text-xs font-medium">全部运行（含临时 / Agent 调用）</div>
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  void flowAPI
                    .allRuns()
                    .then(setAllRuns)
                    .catch((e) => setError(e.message))
                }
              >
                刷新运行
              </Button>
              {allRuns.slice(0, 30).map((item) => (
                <button
                  key={item.id}
                  className="block w-full rounded border p-2 text-left text-xs"
                  onClick={() => {
                    setEvents([]);
                    setRun(null);
                    setRunID(item.id);
                    setInput(pretty(item.input));
                  }}
                >
                  {item.definition.name} · {item.source ?? "manual"}
                  <br />
                  {item.status} · {new Date(item.created_at).toLocaleTimeString()}
                </button>
              ))}
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
                  <Button variant="outline" onClick={() => void action("validate")} disabled={busy}>
                    校验
                  </Button>
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
            {saved && (
              <section className="rounded-xl border bg-card p-3 space-y-2 text-xs">
                <div className="flex gap-3 items-center">
                  <a className="text-primary underline" href="/system/tools">
                    在自定义工具中管理发布工具
                  </a>
                  {saved.published && (
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() =>
                        void flowAPI
                          .unpublish(saved.id)
                          .then(async () => {
                            const items = await flowAPI.list();
                            setRows(items);
                            setSaved(items.find((item) => item.id === saved.id) ?? null);
                            setNotice("已下架，禁止新调用；历史版本和记录保留");
                          })
                          .catch((e) => setError(e.message))
                      }
                    >
                      下架工具
                    </Button>
                  )}
                </div>
                <details>
                  <summary>发布版本历史（{versions.length}）</summary>
                  {versions.map((version) => (
                    <div key={version.id} className="flex items-center justify-between border-t py-2">
                      <span>
                        v{version.revision} · {new Date(version.created_at).toLocaleString()}
                      </span>
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            setDraft(version.definition);
                            setMode("graph");
                            setNotice(`已载入 v${version.revision} 为编辑草稿，尚未保存`);
                          }}
                        >
                          载入编辑
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() =>
                            void flowAPI
                              .rollback(saved.id, version.id)
                              .then((value) => {
                                setSaved(value);
                                setRows((items) => items.map((item) => (item.id === value.id ? value : item)));
                                setNotice(`工具已回滚到 v${version.revision}`);
                              })
                              .catch((e) => setError(e.message))
                          }
                        >
                          回滚工具
                        </Button>
                      </div>
                    </div>
                  ))}
                </details>
              </section>
            )}
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
                    {(["tool", "transform", "branch", "parallel", "join", "end"] as const).map((type) => (
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
                                nodes: current.nodes.map((original) => {
                                  const node =
                                    original.type === "join"
                                      ? {
                                          ...original,
                                          from: original.from?.filter(
                                            (id) =>
                                              !deleted.some(
                                                (edge) => edge.source === id && edge.target === original.id,
                                              ),
                                          ),
                                        }
                                      : original;
                                  const edge = deleted.find((item) => item.source === node.id);
                                  if (!edge) return node;
                                  if (node.type === "parallel")
                                    return {
                                      ...node,
                                      branches: (node.branches ?? []).filter(
                                        (target) =>
                                          !deleted.some((item) => item.source === node.id && item.target === target),
                                      ),
                                    };
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
                      key={saved?.id ?? "new"}
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
                          <label className="block space-y-1 text-xs">
                            节点 ID
                            <Input
                              aria-label="节点 ID"
                              key={active.id}
                              defaultValue={active.id}
                              className="font-mono"
                              onBlur={(event) => {
                                try {
                                  const id = event.target.value.trim();
                                  setDraft(renameFlowNode(draft, active.id, id));
                                  setSelected(id);
                                } catch (error) {
                                  event.target.value = active.id;
                                  setError((error as Error).message);
                                }
                              }}
                            />
                          </label>
                        </div>
                        {(active.type === "parallel" || active.type === "join") && (
                          <label className="block text-xs space-y-2">
                            {active.type === "parallel"
                              ? "并行分支入口 ID（逗号分隔）"
                              : "等待的上游节点 ID（逗号分隔）"}
                            <Input
                              value={(active.type === "parallel" ? active.branches : active.from)?.join(",") ?? ""}
                              onChange={(event) =>
                                updateNode({
                                  [active.type === "parallel" ? "branches" : "from"]: event.target.value
                                    .split(",")
                                    .map((s) => s.trim()),
                                })
                              }
                              onBlur={() =>
                                updateNode({
                                  [active.type === "parallel" ? "branches" : "from"]:
                                    (active.type === "parallel" ? active.branches : active.from)?.filter(Boolean) ?? [],
                                })
                              }
                            />
                          </label>
                        )}
                        {active.type === "transform" && (
                          <>
                            <label className="block text-xs">
                              转换操作
                              <select
                                aria-label="转换操作"
                                className="h-9 w-full rounded border bg-background"
                                value={active.operation ?? "json.parse"}
                                onChange={(event) => updateNode({ operation: event.target.value })}
                              >
                                {catalog?.operations.map((op) => (
                                  <option key={op}>{op}</option>
                                ))}
                              </select>
                            </label>
                            <JsonField
                              key={`${active.id}-transform`}
                              label="转换参数 JSON"
                              value={active.arguments}
                              onCommit={(value) => updateNode({ arguments: value as Record<string, unknown> })}
                              onError={setError}
                            />
                            <p className="text-xs text-muted-foreground">
                              text：文本；value：JSON 值；artifact：工件对象或 ID。结果引用 steps.节点ID.data。
                            </p>
                          </>
                        )}
                        {active.type === "tool" && (
                          <>
                            <a
                              className="text-xs text-primary underline"
                              href="/system/tools"
                              target="_blank"
                              rel="noreferrer"
                            >
                              添加 command / shell 工具（如 nmap），返回后刷新目录
                            </a>
                            <Input
                              aria-label="搜索节点工具"
                              placeholder="搜索工具（例如 nmap）"
                              value={toolQuery}
                              onChange={(event) => setToolQuery(event.target.value)}
                            />
                            <Button
                              size="sm"
                              variant="outline"
                              onClick={() =>
                                void flowAPI
                                  .catalog()
                                  .then((value) => {
                                    setCatalog(value);
                                    setNotice(`已刷新 ${value.tools.length} 个可调用工具`);
                                  })
                                  .catch((err) => setError(err.message))
                              }
                            >
                              刷新工具目录
                            </Button>
                            <label className="block space-y-1 text-xs">
                              工具
                              <select
                                aria-label="节点工具"
                                className="h-9 w-full rounded-md border bg-background px-2"
                                value={active.tool ?? ""}
                                onChange={(event) => {
                                  const tool = catalog?.tools.find((item) => item.name === event.target.value);
                                  updateNode({
                                    tool: event.target.value,
                                    arguments: tool?.input_schema
                                      ? JSON.parse(exampleInput({ input_schema: tool.input_schema } as FlowDefinition))
                                      : {},
                                  });
                                }}
                              >
                                {catalog?.tools
                                  .filter(
                                    (item) =>
                                      item.name === active.tool ||
                                      `${item.name} ${item.description}`
                                        .toLowerCase()
                                        .includes(toolQuery.toLowerCase()),
                                  )
                                  .map((item) => (
                                    <option key={item.name} value={item.name}>
                                      {item.name}
                                    </option>
                                  ))}
                              </select>
                            </label>
                            <p className="text-xs text-muted-foreground">
                              {catalog?.tools.find((item) => item.name === active.tool)?.description}
                            </p>
                            <details className="text-xs">
                              <summary>参数 Schema / 输入类型</summary>
                              <pre className="max-h-52 overflow-auto whitespace-pre-wrap">
                                {pretty(catalog?.tools.find((item) => item.name === active.tool)?.input_schema)}
                              </pre>
                            </details>
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
                              setDraft(removeFlowNodes(draft, [active.id]));
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
                      {runID && !runs.some((item) => item.id === runID) && (
                        <option value={runID}>
                          当前运行 · {run?.source === "inline" ? "临时" : run?.source === "tool" ? "工具调用" : "手动"}{" "}
                          · {run?.status ?? "连接中"}
                        </option>
                      )}
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
                  <Button
                    variant="outline"
                    disabled={busy || run?.status === "running"}
                    onClick={() => void action("inline")}
                  >
                    临时运行
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
                  试运行校验当前草稿，保存后执行；临时运行不增加工作流或工具。运行最长 5 分钟，所有过程落盘。
                </p>
              </div>
              <div className="min-w-0 space-y-2">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <h2 className="text-sm font-semibold">
                    节点执行过程{" "}
                    {run && <span className="ml-2 font-mono text-xs text-muted-foreground">{run.status}</span>}
                  </h2>
                  <div className="flex gap-2 items-center">
                    <Button size="sm" variant="ghost" onClick={() => setShowNative((value) => !value)}>
                      {showNative ? "显示节点过程" : `原生事件 (${events.length})`}
                    </Button>
                    <span className="text-xs text-muted-foreground">{connection}</span>
                  </div>
                </div>
                <div
                  aria-label="节点执行日志"
                  className="h-44 overflow-auto rounded-lg border bg-muted/30 p-2 font-mono text-xs"
                >
                  {visibleEvents.length ? (
                    visibleEvents.map((item) => (
                      <div key={item.id} className="border-b border-border/40 py-1">
                        <span className="text-muted-foreground">{new Date(item.created_at).toLocaleTimeString()} </span>
                        {item.run_id !== runID && (
                          <button
                            className="text-primary underline"
                            onClick={() => {
                              setEvents([]);
                              setRun(null);
                              setRunID(item.run_id);
                            }}
                          >
                            子流程 {item.run_id.slice(0, 8)} ·{" "}
                          </button>
                        )}
                        <details>
                          <summary>
                            {showNative
                              ? `${item.author} · ${item.object}`
                              : `${item.author.replace(/^(node_|tool_)/, "")} · ${item.object === "graph.node.start" ? "开始" : item.object === "graph.node.complete" ? "完成" : "失败"}`}
                          </summary>
                          <pre className="max-h-64 overflow-auto whitespace-pre-wrap">{pretty(item.payload ?? {})}</pre>
                        </details>
                        {item.error && <span className="text-red-600"> {item.error}</span>}
                      </div>
                    ))
                  ) : (
                    <p className="p-3 text-muted-foreground">运行后在这里实时查看节点开始、完成或失败。</p>
                  )}
                </div>
                {run?.parent_run_id && (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => {
                      setEvents([]);
                      setRun(null);
                      setRunID(run.parent_run_id!);
                    }}
                  >
                    返回父流程运行
                  </Button>
                )}
                {run && (
                  <details className="text-xs rounded border p-2">
                    <summary>
                      运行输入与冻结定义 · {run.source} · {run.agent_key || "手动"}
                    </summary>
                    <pre className="max-h-64 overflow-auto whitespace-pre-wrap">
                      {pretty({ input: run.input, definition: run.definition })}
                    </pre>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        const name = window.prompt("另存为工作流名称", `${run.definition.name}_copy`);
                        if (name)
                          void flowAPI
                            .saveRun(run.id, name)
                            .then((value) => {
                              setRows((items) => [value, ...items]);
                              openFlow(value);
                              setNotice("已另存为草稿");
                            })
                            .catch((e) => setError(e.message));
                      }}
                    >
                      另存为工作流
                    </Button>
                  </details>
                )}
                {artifacts.length > 0 && (
                  <div className="rounded border p-2 text-xs space-y-1">
                    <div>完整输出文件</div>
                    {artifacts.map((artifact) => (
                      <a
                        key={artifact.id}
                        className="block text-primary underline"
                        href={sseUrl(`/api/flow-artifacts/${artifact.id}`)}
                        download
                      >
                        {artifact.media_type || "文件"} · {artifact.size} 字节 · {artifact.complete ? "完整" : "不完整"}{" "}
                        · {artifact.id.slice(0, 8)}
                      </a>
                    ))}
                  </div>
                )}
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
