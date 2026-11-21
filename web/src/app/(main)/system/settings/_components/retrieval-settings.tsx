import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { Settings2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { SemanticConfig } from "@/components/semantic-config";
import { learningAPI, type EmbeddingConfig, type RetrievalStatus } from "@/lib/learning-api";

const labels = { none: "基础检索", local: "本机模型检索", remote: "Embedding API 检索" };
export default function RetrievalSettings() {
  const [saved, setSaved] = useState<EmbeddingConfig | null>(null);
  const [draft, setDraft] = useState<EmbeddingConfig | null>(null);
  const [status, setStatus] = useState<RetrievalStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const loadStatus = useCallback(async () => {
    try {
      setStatus(await learningAPI.retrievalStatus());
    } catch {
      /* main load reports errors */
    }
  }, []);
  useEffect(() => {
    let live = true;
    void learningAPI
      .config()
      .then((c) => {
        if (live) {
          setSaved(c);
          setDraft(c);
        }
      })
      .catch((e) => {
        if (live) setError(e instanceof Error ? e.message : "配置读取失败");
      });
    void loadStatus();
    const timer = setInterval(() => void loadStatus(), 3000);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [loadStatus]);
  async function save(value: EmbeddingConfig) {
    setBusy(true);
    try {
      const next = await learningAPI.configure(value);
      setSaved(next);
      setDraft(next);
      setError("");
      await loadStatus();
      toast.success("已保存，任务记忆、全局经验和知识库共用此检索配置");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "保存失败");
    } finally {
      setBusy(false);
    }
  }
  async function test() {
    if (!draft) return false;
    setBusy(true);
    try {
      const checked = await learningAPI.testConfig(draft);
      setDraft((previous) => (previous ? { ...previous, dimensions: checked.dimensions } : previous));
      toast.success(`模型连接成功，检测到 ${checked.dimensions} 维向量`);
      return true;
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "测试失败");
      return false;
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-6">
      <div>
        <h2 className="flex items-center gap-2 text-lg font-semibold">
          <Settings2 className="size-5" />
          模型与检索
        </h2>
        <p className="mt-2 text-sm text-muted-foreground">
          统一管理任务记忆、全局经验和知识库使用的检索方式。这里配置的向量模型用于查找资料，与 LLM 页面的对话模型独立。
        </p>
      </div>
      <div className="space-y-2 rounded-xl border bg-muted/30 p-4" role="status">
        <p className="font-medium">当前生效：{saved ? labels[saved.mode ?? "none"] : "读取中…"}</p>
        <p className="text-sm text-muted-foreground">{status?.message ?? "正在读取运行状态…"}</p>
        {saved && saved.mode !== "none" && (
          <Button variant="outline" disabled={busy} onClick={() => void save({ ...saved, mode: "none" })}>
            停用语义检索，使用基础检索
          </Button>
        )}
        <p className="text-sm">作用范围：任务记忆 · 全局经验 · 知识库。共用模型，数据按各自范围隔离。</p>
      </div>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      {draft && saved && (
        <SemanticConfig
          value={draft}
          activeMode={saved.mode ?? "none"}
          onChange={(next) => setDraft({ ...next, configured: false })}
          onSave={() => void save(draft)}
          onTest={test}
          busy={busy}
        />
      )}
      <div className="flex flex-wrap gap-4 text-sm">
        <Link className="text-primary underline" to="/system/memories">
          记忆设置与全局经验
        </Link>
        <Link className="text-primary underline" to="/system/knowledge">
          知识库与资料导入
        </Link>
      </div>
    </div>
  );
}
