import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { CheckCircle2, Cloud, Cpu, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { learningAPI, type ComponentStatus, type EmbeddingConfig } from "@/lib/learning-api";

function profile(c: EmbeddingConfig) { return JSON.stringify([c.mode, c.base_url, c.model, c.api_key ?? "", c.version ?? ""]); }
export function SemanticConfig({ value, activeMode, onChange, onSave, onTest, busy }: {
  value: EmbeddingConfig; activeMode: "none" | "local" | "remote";
  onChange: (config: EmbeddingConfig) => void; onSave: () => void; onTest: () => Promise<boolean>; busy: boolean;
}) {
  const [component, setComponent] = useState<ComponentStatus | null>(null);
  const [installing, setInstalling] = useState(false);
  const [error, setError] = useState("");
  const [checked, setChecked] = useState(value.configured ? profile(value) : "");
  const file = useRef<HTMLInputElement>(null);
  useEffect(()=>{if(value.configured&&activeMode===value.mode)setChecked(profile(value));},[value,activeMode]);
  useEffect(() => {
    let live = true;
    async function load() { try { const next = await learningAPI.component(); if (live) { setComponent(next); setError(""); } } catch (e) { if (live) setError(e instanceof Error ? e.message : "读取安装状态失败"); } }
    void load(); const timer = setInterval(() => void load(), 2000);
    return () => { live = false; clearInterval(timer); };
  }, []);
  async function install(action: () => Promise<ComponentStatus>) {
    setInstalling(true);
    try { setComponent(await action()); toast.success("正在安装与自检，完成后请点击启用"); }
    catch (e) { toast.error(e instanceof Error ? e.message : "安装失败"); }
    finally { setInstalling(false); }
  }
  function choose(mode: "none" | "local" | "remote") {
    if (mode === value.mode) return;
    if (mode === "none") { onChange({ ...value, mode }); return; }
    if (mode === "remote" && value.base_url) { onChange({ ...value, mode }); return; }
    onChange({ mode, base_url: "", api_key: "", version: "", dimensions: mode === "local" ? 384 : 0, model: mode === "local" ? "multilingual-e5-small" : "", configured: false });
  }
  const locked = busy || installing || !!component?.installing;
  const canInstall = !locked && activeMode === "none";
  const tested = checked === profile(value) && value.dimensions > 0;
  const choices = [
    { mode: "none" as const, title: "基础检索", icon: Search, description: "无需模型或 API。记忆与文档立即可用，适合先开始使用。" },
    { mode: "local" as const, title: "本机模型检索", icon: Cpu, description: "安装模型后在本机生成向量，无需 API Key，提升语义匹配。" },
    { mode: "remote" as const, title: "Embedding API 检索", icon: Cloud, description: "连接已有的内网或在线模型服务，本机无需运行向量模型。" },
  ];
  return <div className="space-y-5">
    <div><h2 className="font-semibold">1. 选择检索方式</h2><p className="mt-1 text-sm text-muted-foreground">以下选择需要保存才会生效，当前生效方式显示在页面顶部。</p></div>
    <div role="radiogroup" aria-label="检索方式" className="grid gap-3 lg:grid-cols-3">
      {choices.map(choice => <button key={choice.mode} type="button" role="radio" aria-checked={(value.mode ?? "none") === choice.mode} disabled={locked} onClick={() => choose(choice.mode)} className={`min-w-0 space-y-2 rounded-xl border p-4 text-left transition-colors disabled:opacity-50 ${(value.mode ?? "none") === choice.mode ? "border-primary bg-primary/5 ring-1 ring-primary" : "hover:bg-muted/40"}`}>
        <span className="flex items-center gap-2 font-medium"><choice.icon className="size-4" />{choice.title}{(value.mode ?? "none") === choice.mode && <CheckCircle2 className="ml-auto size-4 text-primary" />}</span>
        <span className="block text-sm leading-6 text-muted-foreground">{choice.description}</span>
      </button>)}
    </div>
    {value.mode === "none" || !value.mode ? <div className="space-y-3 rounded-xl border p-5"><h2 className="font-semibold">2. 直接使用</h2><p className="text-sm text-muted-foreground">不需要安装组件、填写密钥或选择数据库。稍后可以开启语义检索并补建索引。</p><Button disabled={locked} onClick={onSave}>保存并使用基础检索</Button></div> : <>
      <div className="space-y-4 rounded-xl border p-5">
        <h2 className="font-semibold">2. {value.mode === "local" ? "安装本机模型" : "准备本地索引组件"}</h2>
        <p className="text-sm text-muted-foreground">{value.mode === "local" ? "使用 multilingual-e5-small 模型。首次在线安装下载约 129 MiB，无需配置模型名或 API Key。" : "只安装轻量索引组件，模型运行在你配置的 API 服务上。"}</p>
        <p className="flex items-center gap-2 text-sm">{(value.mode === "local" ? component?.model_installed : component?.installed) ? <><CheckCircle2 className="size-4 text-green-600" />已安装并通过自检</> : "尚未安装"}</p>
        {component?.installing && <div role="status" className="space-y-2"><p className="text-sm">{component.stage} · {(component.downloaded / 1048576).toFixed(1)} / {(component.total / 1048576).toFixed(1)} MiB</p><progress className="h-2 w-full" max={component.total || 1} value={component.downloaded} /></div>}
        {(component?.error || error) && <p role="alert" className="text-sm text-destructive">{component?.error || error}</p>}
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="outline" disabled={!canInstall || !component?.available || (value.mode === "remote" && !!component?.installed)} onClick={() => void install(() => learningAPI.installComponent(value.mode === "local"))}>{value.mode === "local" ? (component?.model_installed ? "重新安装本机模型" : "下载并安装本机模型") : (component?.installed ? "索引组件已就绪" : "安装索引组件")}</Button>
          <Button type="button" variant="outline" disabled={!canInstall} onClick={() => file.current?.click()}>导入离线模型组件包</Button>
          <input ref={file} type="file" accept=".zip" aria-label="选择离线模型组件包" className="hidden" onChange={e => { const selected = e.target.files?.[0]; e.target.value = ""; if (selected) void install(() => learningAPI.importComponent(selected, value.mode === "local")); }} />
        </div>
        {activeMode !== "none" && <p className="text-sm text-muted-foreground">安装或升级前，请先点击页面顶部的“停用语义检索”。</p>}
        {!component?.available && <p className="text-sm text-muted-foreground">此发行包未附带在线安装文件，请导入管理员提供的离线模型组件包。</p>}
      </div>
      {value.mode === "remote" && <div className="space-y-4 rounded-xl border p-5">
        <h2 className="font-semibold">3. 填写并测试 API</h2>
        <p className="text-sm text-muted-foreground">使用兼容 OpenAI Embeddings 接口的服务。地址填写 API 基础地址，通常以 /v1 结尾；模型名填写服务商提供的向量模型名称。</p>
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-2"><Label htmlFor="embedding-url">API 基础地址</Label><Input id="embedding-url" type="url" value={value.base_url} placeholder="https://你的服务地址/v1" onChange={e => onChange({ ...value, base_url: e.target.value, dimensions: 0 })} /><p className="text-xs text-muted-foreground">无需填写末尾的 /embeddings。</p></div>
          <div className="space-y-2"><Label htmlFor="embedding-key">API Key</Label><Input id="embedding-key" type="password" autoComplete="new-password" value={value.api_key ?? ""} placeholder="无认证服务可留空" onChange={e => onChange({ ...value, api_key: e.target.value })} /><p className="text-xs text-muted-foreground">同地址留空会保留已保存密钥。</p></div>
          <div className="space-y-2"><Label htmlFor="embedding-model">向量模型名称</Label><Input id="embedding-model" value={value.model} placeholder="服务商提供的 Embedding 模型名称" onChange={e => onChange({ ...value, model: e.target.value, dimensions: 0 })} /><p className="text-xs text-muted-foreground">应填写向量模型，不是聊天模型。</p></div>
        </div>
        <Button type="button" variant="outline" disabled={locked || !value.base_url || !value.model} onClick={() => void onTest().then(ok => { if (ok) setChecked(profile(value)); })}>测试 API 并检测维度</Button>
        {tested && <p role="status" className="text-sm text-green-700">连接已验证，模型返回 {value.dimensions} 维向量。</p>}
        <p className="text-sm text-muted-foreground">启用后，待编码的记忆、文档片段和问题会发送至该 API；系统不会自动切换服务或使用聊天模型的密钥。</p>
      </div>}
      <div className="space-y-3 rounded-xl border p-5">
        <h2 className="font-semibold">{value.mode === "local" ? "3" : "4"}. 启用并补建索引</h2>
        <p className="text-sm text-muted-foreground">{value.mode === "local" ? "模型按需加载，空闲五分钟释放内存。安装不会自动启用。" : "测试通过后启用。"}已有记忆和文档会后台补建向量索引，无需重新导入；故障时使用基础检索。</p>
        <Button disabled={locked || (value.mode === "local" ? !component?.model_installed : !component?.installed || !tested)} onClick={onSave}>{value.mode === "local" ? "保存并启用本机模型检索" : "保存并启用 API 检索"}</Button>
        {activeMode === "local" && <p className="text-sm text-muted-foreground">模型状态：{component?.model_loaded ? "已加载" : "等待按需加载"}</p>}
      </div>
    </>}
    <details className="rounded-xl border p-4"><summary className="cursor-pointer text-sm">高级信息</summary><div className="mt-3 space-y-3 text-sm text-muted-foreground"><p>向量索引：SQLiteVec，程序自动管理。记忆与文档使用各自的原生适配器。</p><p>平台：{component?.platform ?? "读取中"} · 已检测向量维度：{value.dimensions || "尚未检测"}</p>{value.mode === "remote" && <div className="space-y-2"><Label htmlFor="embedding-version">部署版本 / 模型摘要（可选）</Label><Input id="embedding-version" value={value.version ?? ""} placeholder="相同模型名更换权重时更新，用于重建索引" onChange={e => onChange({ ...value, version: e.target.value })} /></div>}</div></details>
  </div>;
}
