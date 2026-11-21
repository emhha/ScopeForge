import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";
import { Brain, RefreshCw, Sparkles, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import { learningAPI, type MemoryEntry } from "@/lib/learning-api";

export function MemoryPanel({ taskId }: { taskId?: string }) {
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [content, setContent] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    try {
      setEntries(await learningAPI.memories(taskId));
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [taskId]);
  useEffect(() => {
    setLoading(true);
    void load();
  }, [load]);
  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    try {
      await fn();
      await load();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "操作失败");
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-4 p-4 md:p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <Brain className="size-5" />
            {taskId ? "任务记忆" : "全局经验记忆"}
          </h2>
          <p className="mt-1 text-sm text-muted-foreground">
            {taskId
              ? "本任务所有 Agent 共享，与其他任务隔离。默认在任务阶段结束后自动提炼全局经验，也可随时主动提炼。"
              : "所有 Agent 共享的抽象方法。任务中的发现和具体目标保留在任务记忆中，凭据不应写入记忆。"}
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" disabled={busy} onClick={() => void load()}>
            <RefreshCw className="size-4" />
            刷新
          </Button>
          {taskId && (
            <Button
              disabled={busy || entries.length === 0}
              onClick={() =>
                void act(async () => {
                  const result = await learningAPI.distill(taskId);
                  toast.success(
                    result.saved.length
                      ? `已提炼 ${result.saved.length} 条全局经验`
                      : "没有新的经验通过抽象性检查，未写入全局记忆",
                  );
                })
              }
            >
              <Sparkles className="size-4" />
              {busy ? "处理中…" : "提炼为全局经验"}
            </Button>
          )}
        </div>
      </div>
      {taskId && (
        <Card>
          <CardContent className="space-y-3 pt-4">
            <label htmlFor="memory-content" className="text-sm font-medium">
              添加任务记忆
            </label>
            <Textarea
              id="memory-content"
              value={content}
              maxLength={4000}
              onChange={(e) => setContent(e.target.value)}
              placeholder="重要事实、已尝试路线、失败原因或待验证假设…"
            />
            <Button
              disabled={busy || !content.trim()}
              onClick={() =>
                void act(async () => {
                  await learningAPI.addMemory(taskId, content);
                  setContent("");
                })
              }
            >
              保存记忆
            </Button>
            <Link to="/system/memories" className="ml-4 text-sm text-primary underline">
              查看全局经验
            </Link>
          </CardContent>
        </Card>
      )}
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      {loading ? (
        <p role="status" className="text-sm text-muted-foreground">
          加载中…
        </p>
      ) : entries.length === 0 && !error ? (
        <div className="rounded-xl border border-dashed p-10 text-center text-sm text-muted-foreground">
          {taskId
            ? "暂无任务记忆。可手动添加，Agent 也可在执行中通过记忆工具保存。"
            : "暂无共享经验。任务阶段结束后会自动提炼，通过抽象性审核的经验会出现在这里。"}
        </div>
      ) : null}
      {entries.map((entry) => (
        <Card key={entry.id}>
          <CardContent className="flex items-start gap-4 pt-4">
            <div className="min-w-0 flex-1">
              <p className="whitespace-pre-wrap break-words text-sm leading-7">{entry.memory.memory}</p>
              <p className="mt-2 text-sm text-muted-foreground">
                {entry.memory.topics?.join(" · ")} · {new Date(entry.updated_at).toLocaleString("zh-CN")}
              </p>
            </div>
            <Button
              variant="ghost"
              size="icon"
              aria-label="删除记忆"
              disabled={busy}
              onClick={() => void act(() => learningAPI.deleteMemory(entry.id, taskId))}
            >
              <Trash2 className="size-4" />
            </Button>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
