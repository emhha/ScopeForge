"use client";

import * as React from "react";
import {
  CheckIcon,
  ChevronDown,
  ChevronRight,
  CrosshairIcon,
  PaperclipIcon,
  ShieldAlertIcon,
  UserIcon,
  ArrowDownIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Markdown } from "@/components/markdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { ApprovalDetail } from "@/components/approval-records";
import type { Activity, InterceptPending } from "@/lib/types";
import { projectStandardChat, projectTrajectory, type ChatNode, type TrajectoryRecord } from "@/features/conversation/projections";
import { ConversationStep } from "@/features/conversation/trajectory-view";

// useInView latches true when the ref'd element first comes within `rootMargin` of
// the enclosing scroll viewport. Blocks that always show their full body (user
// bubbles, answers) use it to defer fetching that body until they're about to be
// seen — so opening a long thread doesn't fire a detail request for every off-
// screen step. Observes the ScrollArea viewport (falls back to eager load when
// IntersectionObserver is unavailable, e.g. SSR).
function useInView(rootMargin = "400px"): [React.RefObject<HTMLDivElement | null>, boolean] {
  const ref = React.useRef<HTMLDivElement | null>(null);
  const [inView, setInView] = React.useState(false);
  React.useEffect(() => {
    if (inView) return; // latch: once seen, stop observing
    const el = ref.current;
    if (!el) return;
    if (typeof IntersectionObserver === "undefined") {
      setInView(true);
      return;
    }
    const root = el.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null;
    const ob = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setInView(true);
      },
      { root, rootMargin },
    );
    ob.observe(el);
    return () => ob.disconnect();
  }, [inView, rootMargin]);
  return [ref, inView];
}

function ActivityTime({ ts }: { ts: string }) {
  const date = new Date(ts);
  if (!ts || Number.isNaN(date.getTime())) return null;
  return (
    <time
      dateTime={date.toISOString()}
      title={date.toLocaleString("zh-CN")}
      className="text-[10px] text-muted-foreground tabular-nums"
      suppressHydrationWarning
    >
      {date.toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      })}
    </time>
  );
}

// InterceptCard renders an inline intercept_request approval card. The pending_id
// is extracted from the summary (format: "工具 X 请求审批 (#N)") so buttons are
// available immediately without waiting for the detail load.
function InterceptCard({ step, getDetail }: { step: Activity; getDetail: (seq: number) => Promise<string> }) {
  // extract pending_id from summary: "工具 Bash 请求审批 (#42)"
  const pendingId = React.useMemo(() => {
    const m = /\(#(\d+)\)/.exec(step.summary);
    return m ? parseInt(m[1], 10) : null;
  }, [step.summary]);

  const toolName = React.useMemo(() => {
    const m = /工具\s+(\S+)\s+请求/.exec(step.summary);
    return m ? m[1] : step.summary;
  }, [step.summary]);

  const [detail, setDetail] = React.useState<Record<string, unknown> | null>(null);
  const [decided, setDecided] = React.useState<"allowed" | "denied" | "timeout" | null>(null);
  const [deciding, setDeciding] = React.useState(false);
  const [expanded, setExpanded] = React.useState(false);
  const [pending, setPending] = React.useState<InterceptPending | null>(null);
  const [statusError, setStatusError] = React.useState("");
  const [retry, setRetry] = React.useState(0);

  // Load persisted detail JSON + check the real current status from the backend
  // so that a page refresh shows the already-decided state instead of re-offering buttons.
  React.useEffect(() => {
    let live = true;
    getDetail(step.seq)
      .then((raw) => {
        if (!live || !raw) return;
        try {
          setDetail(JSON.parse(raw));
        } catch {
          /* ignore */
        }
      })
      .catch(() => {
        /* ignore */
      });
    return () => {
      live = false;
    };
  }, [step.seq, getDetail]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry explicitly reloads the same approval after a failed request.
  React.useEffect(() => {
    if (!pendingId) return;
    let live = true;
    api
      .interceptGetOne(pendingId)
      .then((p) => {
        if (!live) return;
        setPending(p);
        setStatusError("");
        if (p.status !== "pending") setDecided(p.status as "allowed" | "denied" | "timeout");
      })
      .catch((error) => {
        if (live) setStatusError((error as Error).message || "审批详情加载失败");
      });
    return () => {
      live = false;
    };
  }, [pendingId, retry]);

  async function decide(decision: "allowed" | "denied") {
    if (step.inherited || !pendingId || deciding) return;
    setDeciding(true);
    try {
      await api.interceptDecide(pendingId, decision);
      setDecided(decision);
      toast.success(decision === "allowed" ? "已允许执行" : "已拒绝执行");
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setDeciding(false);
    }
  }

  const inputStr = detail?.input ? JSON.stringify(detail.input).slice(0, 200) : null;

  const row = pending
    ? { ...pending, status: decided || pending.status, conv_title: "", conv_agent_key: "", rule_name: "" }
    : null;

  return (
    <div className="my-2 rounded-lg border border-amber-400/50 bg-amber-50/40 dark:bg-amber-950/15 p-3 text-xs">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex items-start gap-2 min-w-0">
          <ShieldAlertIcon className="mt-0.5 h-3.5 w-3.5 shrink-0 text-amber-500" />
          <div className="min-w-0 space-y-0.5">
            <div className="flex items-center gap-1.5 font-medium">
              <span className="text-amber-700 dark:text-amber-400">审批请求</span>
              <code className="rounded bg-amber-100 dark:bg-amber-900/50 px-1 font-mono text-amber-800 dark:text-amber-300">
                {toolName}
              </code>
              {pendingId && <span className="text-muted-foreground">#{pendingId}</span>}
            </div>
            {inputStr && <p className="font-mono text-muted-foreground truncate">{inputStr}</p>}
          </div>
        </div>

        {step.inherited ? (
          <Badge variant="outline">历史记录 · 只读</Badge>
        ) : decided ? (
          <span
            className={
              "shrink-0 rounded px-2 py-0.5 text-[11px] font-medium " +
              (decided === "allowed"
                ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-400"
                : decided === "timeout"
                  ? "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400"
                  : "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-400")
            }
          >
            {decided === "allowed" ? "已允许" : decided === "timeout" ? "已超时" : "已拒绝"}
          </span>
        ) : (
          <div className="flex shrink-0 gap-1.5">
            <Button
              size="sm"
              className="h-6 px-2 text-xs"
              disabled={deciding || !pendingId}
              onClick={() => decide("allowed")}
            >
              <CheckIcon className="h-3 w-3" />
              允许
            </Button>
            <Button
              size="sm"
              variant="destructive"
              className="h-6 px-2 text-xs"
              disabled={deciding || !pendingId}
              onClick={() => decide("denied")}
            >
              <XIcon className="h-3 w-3" />
              拒绝
            </Button>
          </div>
        )}
      </div>
      {pendingId ? (
        <Collapsible open={expanded} onOpenChange={setExpanded} className="mt-2 min-w-0">
          <CollapsibleTrigger asChild>
            <Button variant="ghost" size="sm" aria-label={expanded ? "收起审批详情" : "展开审批详情"}>
              {expanded ? <ChevronDown data-icon="inline-start" /> : <ChevronRight data-icon="inline-start" />}
              {expanded ? "收起审批详情" : "展开审批详情"}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            {expanded && row ? (
              <ApprovalDetail
                row={row}
                busy={deciding}
                decide={(_id, decision) => decide(decision)}
                revision={retry}
                readOnly={Boolean(step.inherited)}
                defaultExpanded
                onResolved={setDecided}
              />
            ) : statusError ? (
              <div className="flex flex-wrap items-center gap-2 p-3" role="alert">
                <span>{statusError}</span>
                <Button variant="outline" size="sm" onClick={() => setRetry((value) => value + 1)}>
                  重试详情
                </Button>
              </div>
            ) : (
              <p className="p-3 text-muted-foreground">正在加载审批详情…</p>
            )}
          </CollapsibleContent>
        </Collapsible>
      ) : null}
    </div>
  );
}

function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

type MsgAttachment = { name: string; path: string; size: number };

// parseUserBody splits a user turn's body into text + attachments. The backend stores
// Detail as JSON {text, attachments} when files were uploaded, else plain text — so we
// parse defensively and fall back to treating the whole body as text.
function parseUserBody(body: string): { text: string; attachments: MsgAttachment[] } {
  if (body.startsWith("{")) {
    try {
      const p = JSON.parse(body);
      if (p && Array.isArray(p.attachments)) {
        return { text: typeof p.text === "string" ? p.text : "", attachments: p.attachments };
      }
    } catch {
      /* plain text that merely starts with "{" */
    }
  }
  return { text: body, attachments: [] };
}

function UserRow({
  step,
  intent,
  getDetail,
}: {
  step: Activity;
  intent?: boolean;
  getDetail: (seq: number) => Promise<string>;
}) {
  const Icon = intent ? CrosshairIcon : UserIcon;
  const [ref, inView] = useInView();
  // Optimistic echoes carry their detail inline; persisted rows lazy-load it on scroll.
  const inline = step.detail && step.detail.length > 0 ? step.detail : null;
  const [full, setFull] = React.useState<string | null>(inline);
  React.useEffect(() => {
    if (inline) return; // already have the body (optimistic echo)
    if (!inView) return; // fetch the full message only when the bubble nears view
    let live = true;
    getDetail(step.seq)
      .then((d) => {
        if (live) setFull(d || step.summary);
      })
      .catch(() => {
        if (live) setFull(step.summary);
      });
    return () => {
      live = false;
    };
  }, [inView, step.seq, getDetail, step.summary, inline]);
  const { text, attachments } = parseUserBody(full ?? step.summary);
  return (
    <div ref={ref} className="mt-3 mb-2 flex min-w-0 justify-end gap-2">
      <div className="flex min-w-0 max-w-[85%] flex-col items-end gap-1.5">
        {attachments.length > 0 && (
          <div className="flex min-w-0 max-w-full flex-wrap justify-end gap-1.5">
            {attachments.map((a) => (
              <div
                key={a.path}
                className="flex min-w-0 max-w-full items-center gap-1.5 rounded-md border bg-card px-2 py-1 text-xs shadow-sm"
                title={a.path}
              >
                <PaperclipIcon className="size-3 shrink-0 text-primary" />
                <span className="max-w-[180px] truncate font-medium">{a.name}</span>
                <span className="shrink-0 text-muted-foreground">{fmtBytes(a.size)}</span>
              </div>
            ))}
          </div>
        )}
        {text && (
          <div className="min-w-0 max-w-full whitespace-pre-wrap rounded-lg rounded-tr-sm bg-primary px-3 py-1.5 text-sm text-primary-foreground [overflow-wrap:anywhere]">
            {text}
          </div>
        )}
        <ActivityTime ts={step.ts} />
      </div>
      <div className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10">
        <Icon className="size-3.5 text-primary" />
      </div>
    </div>
  );
}

// AnswerBlock renders the agent's FINAL answer (kind="result") in full — never
// collapsed. The summary is a truncated first line, so the full text is pulled
// from the detail and shown inline.
function AnswerBlock({ step, getDetail }: { step: Activity; getDetail: (seq: number) => Promise<string> }) {
  const [ref, inView] = useInView();
  const [full, setFull] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!inView) return; // fetch the full answer only when it nears view
    let live = true;
    getDetail(step.seq)
      .then((d) => {
        if (live) setFull(d || step.summary);
      })
      .catch(() => {
        if (live) setFull(step.summary);
      });
    return () => {
      live = false;
    };
  }, [inView, step.seq, getDetail, step.summary]);
  return (
    <div ref={ref} className="mb-2 mt-1 flex min-w-0 flex-col gap-1">
      <div
        className={
          "min-w-0 flex-1 break-words rounded-lg bg-muted px-3 py-2 " +
          (step.is_error ? "text-sm text-red-600 dark:text-red-400" : "")
        }
      >
        {step.is_error ? (
          <span className="whitespace-pre-wrap">{full ?? step.summary}</span>
        ) : (
          <Markdown text={full ?? step.summary} />
        )}
      </div>
      <ActivityTime ts={step.ts} />
    </div>
  );
}

function ProcessSummary({ node, records, memory, focusedSeq, getDetail, onReading }: {
  node: Extract<ChatNode, { kind: "process" }>;
  records: TrajectoryRecord[];
  memory: ViewMemory;
  focusedSeq?: number;
  getDetail: (seq: number) => Promise<string>;
  onReading: () => void;
}) {
  const events = new Set(node.events);
  const steps = records.filter((record) => events.has(record.event));
  const focused = focusedSeq != null && steps.some((record) => record.event.seq === focusedSeq || record.result?.seq === focusedSeq);
  const memoryKey = `process:${node.id}`;
  const [open, setOpen] = React.useState(memory.expanded[memoryKey] ?? false);
  React.useEffect(() => {
    if (focused) setOpen(true);
  }, [focused]);
  const tools = steps.filter((record) => record.event.kind === "tool_use" || record.event.kind === "tool_result").length;
  const thinking = node.events.filter((event) => event.kind === "thinking").length;
  const errors = steps.filter((record) => record.status === "error").length;
  if (steps.length === 0) return null;
  return (
    <Collapsible open={open} onOpenChange={(value) => {
      setOpen(value);
      memory.expanded[memoryKey] = value;
      onReading();
    }} className="my-2 min-w-0">
      <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 py-1.5 text-left text-xs text-muted-foreground hover:text-foreground">
        {open ? <ChevronDown className="size-3 shrink-0" /> : <ChevronRight className="size-3 shrink-0" />}
        <span>{node.settled ? "已完成的执行过程" : "执行过程"}</span>
        {tools > 0 && <span>· {tools} 次工具调用</span>}
        {thinking > 0 && <span>· {thinking} 段思考</span>}
        {errors > 0 && <span className="text-destructive">· {errors} 个错误</span>}
      </CollapsibleTrigger>
      <CollapsibleContent>
        {open && <div className="ml-1.5 min-w-0 border-l pl-3">
          {steps.map((record) => (
            <div key={record.id} data-activity-seq={record.event.seq} data-result-seq={record.result?.seq}>
              <ConversationStep
                record={record}
                getDetail={getDetail}
                focused={focusedSeq != null && (record.event.seq === focusedSeq || record.result?.seq === focusedSeq)}
                initiallyOpen={memory.expanded[record.id] ?? false}
                onOpenChange={(value) => {
                  memory.expanded[record.id] = value;
                  onReading();
                }}
              />
            </div>
          ))}
        </div>}
      </CollapsibleContent>
    </Collapsible>
  );
}

type ViewMemory = {
  position?: number;
  following?: boolean;
  expanded: Record<string, boolean>;
};
// Retain only presentation state, never conversation bodies, for recent sessions.
const viewMemories = new Map<string, ViewMemory>();
function viewMemory(scope: string): ViewMemory {
  const memory = viewMemories.get(scope) ?? { expanded: {} };
  viewMemories.delete(scope);
  viewMemories.set(scope, memory);
  if (viewMemories.size > 50) viewMemories.delete(viewMemories.keys().next().value!);
  return memory;
}

interface TranscriptProps {
  activity: Activity[];
  live?: boolean;
  taskId?: string;
  chat?: boolean;
  fetchDetail?: (seq: number) => Promise<string>;
  focusedSeq?: number;
  scopeKey?: string;
  onReading?: () => void;
  onFollow?: () => void;
}

export function Transcript(props: TranscriptProps) {
  const scope = props.scopeKey ?? `task:${props.taskId ?? "local"}`;
  return <TranscriptBody key={scope} {...props} scope={scope} />;
}

function TranscriptBody({
  activity,
  live,
  taskId,
  fetchDetail,
  focusedSeq,
  scope,
  onReading,
  onFollow,
}: TranscriptProps & { scope: string }) {
  const callbacks = React.useRef({ onReading, onFollow });
  callbacks.current = { onReading, onFollow };
  const root = React.useRef<HTMLDivElement>(null);
  const memory = React.useMemo(() => viewMemory(scope), [scope]);
  const [following, setFollowing] = React.useState(memory.following ?? true);
  const followingRef = React.useRef(following);
  const [unread, setUnread] = React.useState(0);
  const lastSeq = React.useRef(activity.at(-1)?.seq ?? 0);
  const detailCache = React.useRef(new Map<number, Promise<string>>());
  const inlineDetails = React.useRef(new Map<number, string>());
  inlineDetails.current = new Map(
    activity.filter((a) => a.detail != null || a.seq < 0).map((a) => [a.seq, a.detail ?? a.summary]),
  );
  const getDetail = React.useCallback(
    (seq: number) => {
      const inline = inlineDetails.current.get(seq);
      if (inline != null) return Promise.resolve(inline);
      let promise = detailCache.current.get(seq);
      if (!promise) {
        promise = (fetchDetail ? fetchDetail(seq) : api.activityDetail(seq, taskId).then((r) => r.detail ?? "")).catch(
          (error) => {
            detailCache.current.delete(seq);
            throw error;
          },
        );
        detailCache.current.set(seq, promise);
        if (detailCache.current.size > 500) detailCache.current.delete(detailCache.current.keys().next().value!);
      }
      return promise;
    },
    [fetchDetail, taskId],
  );
  const getViewport = React.useCallback(
    () => root.current?.closest<HTMLElement>('[data-slot="scroll-area-viewport"]') ?? null,
    [],
  );
  const pause = React.useCallback(() => {
    followingRef.current = false;
    setFollowing(false);
    callbacks.current.onReading?.();
  }, []);
  const follow = React.useCallback(() => {
    followingRef.current = true;
    setFollowing(true);
    setUnread(0);
    callbacks.current.onFollow?.();
    const viewport = getViewport();
    if (viewport) viewport.scrollTop = viewport.scrollHeight;
  }, [getViewport]);
  const savePosition = React.useCallback(() => {
    const viewport = getViewport();
    if (viewport) memory.position = viewport.scrollTop;
    memory.following = followingRef.current;
  }, [getViewport, memory]);
  const focusAvailable = focusedSeq != null && activity.some((event) => event.seq === focusedSeq);
  React.useEffect(() => {
    if (!focusAvailable || focusedSeq == null) return;
    pause();
    const frame = requestAnimationFrame(() => {
      const elements = root.current?.querySelectorAll<HTMLElement>("[data-activity-seq]");
      const target = Array.from(elements ?? []).find((element) =>
        element.dataset.activitySeq === String(focusedSeq) || element.dataset.resultSeq === String(focusedSeq),
      );
      target?.scrollIntoView({ block: "center" });
    });
    return () => cancelAnimationFrame(frame);
  }, [focusedSeq, focusAvailable, pause]);
  React.useEffect(() => {
    const viewport = getViewport();
    if (!viewport) return;
    const onScroll = () => {
      const atBottom = viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 60;
      followingRef.current = atBottom;
      setFollowing(atBottom);
      memory.following = atBottom;
      memory.position = viewport.scrollTop;
      if (atBottom) setUnread(0);
    };
    viewport.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      savePosition();
      viewport.removeEventListener("scroll", onScroll);
    };
  }, [getViewport, memory, savePosition]);
  React.useEffect(() => {
    const content = root.current;
    const viewport = getViewport();
    if (!content || !viewport) return;
    const observer = new ResizeObserver(() => {
      if (followingRef.current) viewport.scrollTop = viewport.scrollHeight;
    });
    observer.observe(content);
    return () => observer.disconnect();
  }, [getViewport]);
  React.useEffect(() => {
    if (focusedSeq != null) return;
    const position = memory.position;
    const shouldFollow = memory.following ?? true;
    const frame = requestAnimationFrame(() => {
      const viewport = getViewport();
      if (!viewport) return;
      if (shouldFollow) follow();
      else {
        pause();
        viewport.scrollTop = position ?? 0;
      }
    });
    return () => cancelAnimationFrame(frame);
  }, [memory, getViewport, focusedSeq, follow, pause]);
  React.useEffect(() => {
    const newest = activity.at(-1)?.seq ?? 0;
    if (newest > lastSeq.current && !followingRef.current) {
      const count = activity.filter((a) => a.seq > lastSeq.current).length;
      setUnread((n) => n + count);
    }
    lastSeq.current = Math.max(newest, lastSeq.current);
  }, [activity]);
  const records = React.useMemo(() => projectTrajectory(scope, activity), [scope, activity]);
  const nodes = React.useMemo(() => projectStandardChat(scope, activity), [scope, activity]);
  return (
    <div ref={root} className="relative min-w-0" data-conversation-view="chat">
      <div className="min-w-0" data-testid="standard-chat-view">
        {nodes.map((node) => {
          if (node.kind === "process") return (
            <ProcessSummary key={node.id} node={node} records={records} memory={memory}
              focusedSeq={focusedSeq} getDetail={getDetail} onReading={pause} />
          );
          const event = node.event;
          const record = records.find((entry) => entry.event === event);
          if (!record) return null;
          return (
            <div key={record.id} data-activity-seq={event.seq} data-result-seq={record.result?.seq}>
              {event.kind === "user" || event.kind === "intent" ? (
                <UserRow step={event} intent={event.kind === "intent"} getDetail={getDetail} />
              ) : event.kind === "text" || event.kind === "result" ? (
                <AnswerBlock step={event} getDetail={getDetail} />
              ) : event.kind === "intercept_request" ? (
                <InterceptCard step={event} getDetail={getDetail} />
              ) : (
                <ConversationStep
                  record={record}
                  getDetail={getDetail}
                  focused={focusedSeq != null && (event.seq === focusedSeq || record.result?.seq === focusedSeq)}
                  initiallyOpen={memory.expanded[record.id] ?? false}
                  onOpenChange={(open) => {
                    pause();
                    memory.expanded[record.id] = open;
                  }}
                />
              )}
            </div>
          );
        })}
        {live && (
          <div role="status" className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
            <span className="size-1.5 animate-pulse rounded-full bg-primary motion-reduce:animate-none" />
            正在处理…
          </div>
        )}
      </div>
      {!following && (
        <div className="pointer-events-none sticky bottom-2 z-20 flex justify-center">
          <Button
            size="sm"
            variant="secondary"
            className="pointer-events-auto h-7 gap-1 shadow-sm"
            onClick={() => {
              follow();
            }}
          >
            <ArrowDownIcon className="size-3" />
            {unread ? `${unread} 条新记录 · ` : ""}回到最新
          </Button>
        </div>
      )}
    </div>
  );
}
