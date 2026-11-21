import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { CopyButton } from "@/components/copy-button";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { Activity } from "@/lib/types";
import { recordedSystemPrompt, type TrajectoryRecord } from "./projections";
import { ChevronDown, ChevronRight, Wrench, BrainIcon, ListIcon } from "lucide-react";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

export const EVENT_LABELS: Record<string, string> = {
  user: "用户输入",
  intent: "探索目标",
  thinking: "思考",
  text: "助手回复",
  result: "回合结果",
  tool_use: "工具调用",
  tool_result: "工具结果",
  model_call: "模型请求",
  round: "规划轮次",
  llm_switch: "模型切换",
  llm_failover: "故障转移",
  intercept_request: "审批请求",
};

function roleOf(event: Activity) {
  return event.metadata?.llm_call?.role ?? (event.worker ? event.worker : "未记录角色");
}

function DetailText({ title, text }: { title: string; text: string }) {
  let formatted = text;
  try {
    formatted = JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    /* Preserve non-JSON output verbatim. */
  }
  return (
    <section className="min-w-0 space-y-1">
      <div className="flex items-center justify-between gap-2">
        <h4 className="font-medium">{title}</h4>
        <CopyButton text={text} label="复制原文" size="sm" variant="ghost" />
      </div>
      <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-md border bg-background p-3 font-mono text-xs">
        {formatted || "（空内容）"}
      </pre>
    </section>
  );
}

function SystemPromptDetail({ prompt }: { prompt: string | undefined }) {
  if (prompt === undefined) {
    return <p className="text-muted-foreground">此历史请求未记录系统提示词；不会使用当前配置代替。</p>;
  }
  if (prompt === "") return <p className="text-muted-foreground">本次请求没有系统提示词。</p>;
  return <DetailText title="系统提示词（实际请求）" text={prompt} />;
}

function EventDetail({
  record,
  getDetail,
  chat = false,
}: {
  record: TrajectoryRecord;
  getDetail: (seq: number) => Promise<string>;
  chat?: boolean;
}) {
  const [state, setState] = useState<{ input?: string; output?: string; error?: string }>({});
  const [retry, setRetry] = useState(0);
  const event = record.event;
  const model = event.metadata?.llm_call;
  const resultSeq = record.result?.seq;
  const systemPrompt = state.input === undefined ? undefined : recordedSystemPrompt(state.input);
  // biome-ignore lint/correctness/useExhaustiveDependencies: retry explicitly reloads the selected record after a failed read.
  useEffect(() => {
    let alive = true;
    setState({});
    Promise.all([
      getDetail(event.seq),
      resultSeq != null && resultSeq !== event.seq ? getDetail(resultSeq) : Promise.resolve(undefined),
    ])
      .then(([input, output]) => {
        if (alive) setState({ input, output });
      })
      .catch((e: Error) => {
        if (alive) setState({ error: e.message });
      });
    return () => {
      alive = false;
    };
  }, [event.seq, event.detail, resultSeq, getDetail, retry, model]);
  return (
    <div className="space-y-3 bg-muted/30 p-3 text-xs">
      {!chat && (
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-muted-foreground">
          <span>记录 #{event.seq}</span>
          <span>{event.ts}</span>
          {event.tool_use_id && <span className="break-all">调用 {event.tool_use_id}</span>}
          {event.inherited && <span>继承自任务 {event.source_task_id}</span>}
        </div>
      )}
      {model &&
        (chat ? (
          <p className="text-muted-foreground">
            {model.profile_name} · {model.model} · 输入 {model.input_tokens ?? "—"} / 输出 {model.output_tokens ?? "—"}{" "}
            tokens
          </p>
        ) : (
          <DetailText title="实际模型请求记录" text={JSON.stringify(model)} />
        ))}
      {state.error ? (
        <div role="alert" className="text-destructive">
          详情加载失败：{state.error}{" "}
          <Button size="sm" variant="outline" onClick={() => setRetry((n) => n + 1)}>
            重试
          </Button>
        </div>
      ) : state.input === undefined ? (
        <p role="status">加载详情…</p>
      ) : event.kind === "model_call" ? (
        <SystemPromptDetail prompt={systemPrompt} />
      ) : chat && event.kind === "thinking" ? (
        <Markdown text={state.input || event.detail || event.summary} />
      ) : (
        <>
          <DetailText
            title={
              event.kind === "tool_use"
                ? "输入参数"
                : event.kind === "tool_result"
                  ? "输出（调用参数尚未加载）"
                  : "完整内容"
            }
            text={state.input || event.detail || event.summary}
          />
          {state.output !== undefined && (
            <DetailText
              title={record.result?.is_error ? "错误输出" : "输出结果"}
              text={state.output || record.result?.summary || ""}
            />
          )}
          {event.kind === "tool_use" && !record.result && (
            <p className="text-muted-foreground">尚未收到结果；不会据此推断执行已结束。</p>
          )}
          {event.metadata?.llm_transition && (
            <DetailText title="模型切换记录" text={JSON.stringify(event.metadata.llm_transition)} />
          )}
          {event.input_tokens != null && (
            <p>
              输入 {event.input_tokens} · 输出 {event.output_tokens ?? "不可用"} · 缓存读取{" "}
              {event.cache_read_tokens ?? "不可用"}
            </p>
          )}
        </>
      )}
    </div>
  );
}

/** A compact chat process step; its details open here instead of navigating. */
export function ConversationStep({
  record,
  getDetail,
  focused,
  initiallyOpen,
  onOpenChange,
}: {
  record: TrajectoryRecord;
  getDetail: (seq: number) => Promise<string>;
  focused: boolean;
  initiallyOpen: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [open, setOpen] = useState(initiallyOpen);
  useEffect(() => {
    if (focused) setOpen(true);
  }, [focused]);
  const event = record.event;
  const model = event.metadata?.llm_call;
  const Icon =
    event.kind === "tool_use" || event.kind === "tool_result"
      ? Wrench
      : event.kind === "thinking"
        ? BrainIcon
        : ListIcon;
  const status = { done: "已完成", error: "错误", interrupted: "已中断", pending: "等待结果", recorded: "已记录" }[
    record.status
  ];
  return (
    <Collapsible
      open={open}
      onOpenChange={(value) => {
        setOpen(value);
        onOpenChange(value);
      }}
      className={`min-w-0 ${focused ? "rounded-md bg-primary/5" : ""}`}
    >
      <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 rounded-md py-1.5 text-left text-xs text-muted-foreground hover:bg-muted/50">
        {open ? (
          <ChevronDown className="mt-0.5 size-3 shrink-0" />
        ) : (
          <ChevronRight className="mt-0.5 size-3 shrink-0" />
        )}
        <Icon className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1 truncate">
          {model ? `模型请求 · ${model.model} · 系统提示词` : event.tool || EVENT_LABELS[event.kind] || event.kind}
          {!model && event.summary && <span className="ml-2 opacity-70">{event.summary}</span>}
        </span>
        {(record.status === "error" || record.status === "pending" || record.status === "interrupted") && (
          <span className={`shrink-0 ${record.status === "error" ? "text-destructive" : ""}`}>{status}</span>
        )}
      </CollapsibleTrigger>
      <CollapsibleContent>{open && <EventDetail record={record} getDetail={getDetail} chat />}</CollapsibleContent>
    </Collapsible>
  );
}

export function TrajectoryView({
  records,
  visible,
  targetSeq,
  getDetail,
  getViewport,
  onReading,
}: {
  records: TrajectoryRecord[];
  visible: boolean;
  targetSeq?: number;
  getDetail: (seq: number) => Promise<string>;
  getViewport: () => HTMLElement | null;
  onReading: () => void;
}) {
  "use no memo";
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState("all");
  const [role, setRole] = useState("all");
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [selected, setSelected] = useState<string>();
  const lastTarget = useRef<number | undefined>(undefined);
  const [jumpTo, setJumpTo] = useState<string>();
  const body = useRef<HTMLTableSectionElement>(null);
  const [margin, setMargin] = useState(0);
  const filtered = useMemo(
    () =>
      records.filter((record) => {
        const e = record.event;
        return (
          (kind === "all" || kind === "tools"
            ? kind === "all" || e.kind === "tool_use" || e.kind === "tool_result"
            : e.kind === kind) &&
          (role === "all" || roleOf(e) === role) &&
          (!errorsOnly || record.status === "error") &&
          (!query ||
            [e.summary, record.result?.summary, e.tool, roleOf(e), e.metadata?.llm_call?.model]
              .join(" ")
              .toLowerCase()
              .includes(query.toLowerCase()))
        );
      }),
    [records, query, kind, role, errorsOnly],
  );
  const virtualizer = useVirtualizer({
    count: filtered.length,
    getScrollElement: getViewport,
    estimateSize: () => 44,
    overscan: 8,
    scrollPaddingStart: 120,
    getItemKey: (index) => filtered[index].id,
    scrollMargin: margin,
    enabled: visible,
    useFlushSync: false,
  });
  useLayoutEffect(() => {
    if (!visible) return;
    const el = body.current;
    const viewport = getViewport();
    if (!el || !viewport) return;
    const measure = () =>
      setMargin(el.getBoundingClientRect().top - viewport.getBoundingClientRect().top + viewport.scrollTop);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(viewport);
    return () => observer.disconnect();
  }, [visible, getViewport]);
  useEffect(() => {
    if (!visible || targetSeq == null) {
      lastTarget.current = undefined;
      return;
    }
    if (lastTarget.current === targetSeq) return;
    const record = records.find((r) => r.event.seq === targetSeq || r.result?.seq === targetSeq);
    if (!record) return;
    lastTarget.current = targetSeq;
    setQuery("");
    setKind("all");
    setRole("all");
    setErrorsOnly(false);
    setSelected(record.id);
    setJumpTo(record.id);
  }, [visible, targetSeq, records]);
  const navigateTo = useCallback(
    (id: string) => {
      const index = filtered.findIndex((r) => r.id === id);
      if (index >= 0) virtualizer.scrollToIndex(index, { align: "start" });
    },
    [filtered, virtualizer],
  );
  useEffect(() => {
    if (!visible || !jumpTo || !filtered.some((r) => r.id === jumpTo)) return;
    onReading();
    const frame = requestAnimationFrame(() => {
      navigateTo(jumpTo);
      setJumpTo(undefined);
    });
    return () => cancelAnimationFrame(frame);
  }, [visible, jumpTo, filtered, navigateTo, onReading]);
  const roles = [...new Set(records.map((r) => roleOf(r.event)))];
  const control = "h-8 max-w-full rounded-md border bg-background px-2 text-xs";
  return (
    <div hidden={!visible} data-testid="trajectory-view" className="min-w-0">
      <div className="sticky top-9 z-10 flex flex-wrap items-center gap-2 border-b bg-background py-2">
        <Input
          aria-label="筛选已加载事件"
          onFocus={onReading}
          placeholder="筛选已加载事件…"
          className="h-8 w-44 text-xs"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <select
          aria-label="事件类型"
          onFocus={onReading}
          className={control}
          value={kind}
          onChange={(e) => setKind(e.target.value)}
        >
          <option value="all">全部类型</option>
          <option value="tools">工具</option>
          <option value="thinking">思考</option>
          <option value="model_call">模型请求</option>
          <option value="intercept_request">审批</option>
          <option value="llm_failover">故障转移</option>
          <option value="llm_switch">模型切换</option>
        </select>
        <select
          aria-label="角色筛选"
          onFocus={onReading}
          className={control}
          value={role}
          onChange={(e) => setRole(e.target.value)}
        >
          <option value="all">全部角色</option>
          {roles.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <label className="flex items-center gap-1 text-xs">
          <input
            type="checkbox"
            onFocus={onReading}
            checked={errorsOnly}
            onChange={(e) => setErrorsOnly(e.target.checked)}
          />
          仅错误
        </label>
        <span className="text-muted-foreground text-xs">
          已加载 {records.length} 条 · 匹配 {filtered.length} 条
        </span>
      </div>
      <table className="block w-full" aria-label="会话轨迹" aria-rowcount={filtered.length + 1}>
        <thead className="block">
          <tr className="grid grid-cols-[5rem_minmax(0,1fr)_5rem] gap-2 border-b py-2 text-muted-foreground text-xs md:grid-cols-[5rem_5rem_minmax(0,1fr)_8rem_5rem]">
            <th className="text-left font-normal">类型</th>
            <th className="hidden text-left font-normal md:block">角色</th>
            <th className="text-left font-normal">摘要</th>
            <th className="hidden text-left font-normal md:block">模型 / 用量</th>
            <th className="text-left font-normal">状态</th>
          </tr>
        </thead>
        <tbody ref={body} className="relative block w-full" style={{ height: virtualizer.getTotalSize() }}>
          {virtualizer.getVirtualItems().map((row) => {
            const record = filtered[row.index];
            const e = record.event;
            const model = e.metadata?.llm_call;
            const status = {
              done: "已完成",
              error: "错误",
              interrupted: "已中断",
              pending: "等待结果",
              recorded: "已记录",
            }[record.status];
            return (
              <tr
                key={record.id}
                data-index={row.index}
                ref={virtualizer.measureElement}
                aria-rowindex={row.index + 2}
                aria-selected={selected === record.id}
                className="absolute top-0 left-0 w-full border-b text-xs"
                style={{ transform: `translateY(${row.start - margin}px)` }}
              >
                <td colSpan={5} className="block w-full p-0">
                  <button
                    type="button"
                    aria-expanded={selected === record.id}
                    aria-label={`查看${EVENT_LABELS[e.kind] ?? e.kind} #${e.seq}`}
                    className={`grid min-h-11 w-full grid-cols-[5rem_minmax(0,1fr)_5rem] items-center gap-2 px-1 text-left hover:bg-muted/60 md:grid-cols-[5rem_5rem_minmax(0,1fr)_8rem_5rem] ${selected === record.id ? "bg-muted" : ""}`}
                    onClick={() => {
                      onReading();
                      setSelected((value) => (value === record.id ? undefined : record.id));
                    }}
                  >
                    <span>{EVENT_LABELS[e.kind] ?? e.kind}</span>
                    <span className="hidden truncate md:block">{roleOf(e)}</span>
                    <span className="min-w-0 truncate" title={e.summary}>
                      {e.summary || e.tool || "—"}
                    </span>
                    <span className="hidden truncate text-muted-foreground md:block" title={model?.model}>
                      {model
                        ? `${model.model} · ${model.input_tokens ?? "—"}/${model.output_tokens ?? "—"}`
                        : e.input_tokens != null
                          ? `${e.input_tokens}/${e.output_tokens ?? "—"}`
                          : "—"}
                    </span>
                    <span className={record.status === "error" ? "text-destructive" : "text-muted-foreground"}>
                      {status}
                    </span>
                  </button>
                  {selected === record.id && <EventDetail record={record} getDetail={getDetail} />}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {filtered.length === 0 && <p className="py-8 text-center text-muted-foreground text-sm">没有匹配的已加载事件</p>}
    </div>
  );
}
