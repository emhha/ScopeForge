import {Link} from "react-router";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { learningAPI, type MemorySettings, type MemoryStatus } from "@/lib/learning-api";

export function MemorySettingsCard() {
  const [status, setStatus] = useState<MemoryStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const next = await learningAPI.memoryStatus();
        if (active) {
          setStatus(next);
          setError("");
        }
      } catch (e) {
        if (active) setError(e instanceof Error ? e.message : "状态读取失败");
      }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, []);
  async function change(key: keyof MemorySettings, value: boolean) {
    if (!status) return;
    setBusy(true);
    try {
      const settings = await learningAPI.memoryConfig({ ...status.settings, [key]: value });
      setStatus({ ...status, settings });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "保存失败");
    } finally {
      setBusy(false);
    }
  }
  const controls: { key: keyof MemorySettings; title: string; description: string }[] = [
    {
      key: "task_enabled",
      title: "任务记忆",
      description: "默认开启。Agent 自主保存重要上下文，同任务共享，跨任务隔离。",
    },
    {key:"checkpoint_enabled",title:"阶段检查点",description:"默认开启。阶段结束后从已落库的摘要和任务图补充提取记忆；失败保留并重试。"},
    {
      key: "global_enabled",
      title: "全局经验记忆",
      description: "默认开启。所有 Agent 共享抽象经验，任务细节保留在任务内。",
    },
    {
      key: "auto_distill",
      title: "自动提炼全局经验",
      description: "默认开启。任务阶段结束后后台提炼、检查和去重，失败自动重试。",
    },
    {
      key: "auto_extract",
      title: "任务会话自动提取（高级）",
      description: "默认关闭。启用原生 Auto 模式补充提取；读取知识库等外部资料的会话会跳过自动提取。",
    },
  ];
  return (
    <Card className="mx-4 mt-4 md:mx-6 md:mt-6">
      <CardHeader>
        <CardTitle>记忆设置</CardTitle>
        <CardDescription>
          {status
            ? `${status.retrieval.message} · ${status.jobs.pending} 个记忆后台作业${status.jobs.retrying ? `，其中 ${status.jobs.retrying} 个等待重试` : ""}`
            : "读取记忆状态…"}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
 <Link to="/system/settings?tab=retrieval" className="text-sm text-primary underline">配置记忆和知识库共用的模型与检索方式</Link>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {controls.map((control) => (
          <div key={control.key} className="flex items-start justify-between gap-4">
            <div>
              <Label htmlFor={`memory-${control.key}`}>{control.title}</Label>
              <p className="mt-1 text-sm text-muted-foreground">{control.description}</p>
            </div>
            <Switch
              id={`memory-${control.key}`}
              checked={status?.settings[control.key] ?? false}
              disabled={!status || busy}
              onCheckedChange={(value) => void change(control.key, value)}
            />
          </div>
        ))}
      </CardContent>
    </Card>
  );
}
