"use client";

import * as React from "react";
import { NetworkIcon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { MappingSource, MappingSourcePatch } from "@/lib/types";

const names = { fofa: "FOFA", hunter: "Hunter", quake: "360 Quake" };

function SourceEditor({ source, onSaved }: { source: MappingSource; onSaved: (source: MappingSource) => void }) {
  const [enabled, setEnabled] = React.useState(source.enabled);
  const [key, setKey] = React.useState("");
  const [email, setEmail] = React.useState(source.email);
  const [endpoint, setEndpoint] = React.useState(source.endpoint);
  const [proxy, setProxy] = React.useState(source.proxy);
  const [timeout, setTimeout] = React.useState(String(source.timeout_seconds));
  const [saving, setSaving] = React.useState(false);
  const label = names[source.id];

  async function save(clearKey = false) {
    const seconds = Number(timeout);
    if (!Number.isInteger(seconds) || seconds < 1 || seconds > 120) {
      toast.error("超时必须为 1–120 秒的整数");
      return;
    }
    const patch: MappingSourcePatch = {
      enabled: clearKey ? false : enabled,
      endpoint: endpoint.trim(), proxy: proxy.trim(), timeout_seconds: seconds,
    };
    if (clearKey || key.trim()) patch.key = clearKey ? "" : key.trim();
    if (source.id === "fofa") patch.email = email.trim();
    setSaving(true);
    try {
      const saved = await api.saveMappingSource(source.id, patch);
      onSaved(saved);
      setKey("");
      setEnabled(saved.enabled);
      setEndpoint(saved.endpoint);
      toast.success(`已保存 ${label} 配置`);
    } catch (error) {
      toast.error("保存失败：" + (error as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <fieldset disabled={saving} className="grid min-w-0 gap-3 rounded-lg border p-3">
      <div className="flex items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Label htmlFor={`mapping-${source.id}-enabled`}>{label}</Label>
          <Badge variant={source.key_set ? "secondary" : "outline"}>{source.key_set ? "已配置 Key" : "未配置 Key"}</Badge>
        </div>
        <Switch id={`mapping-${source.id}-enabled`} checked={enabled} onCheckedChange={setEnabled} aria-label={`启用 ${label}`} />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor={`mapping-${source.id}-key`} className="text-xs">API Key</Label>
        <Input id={`mapping-${source.id}-key`} type="password" autoComplete="new-password" value={key}
          onChange={(event) => setKey(event.target.value)} placeholder={source.key_set ? "已保存；留空保持原值" : "输入 API Key"} />
        {source.key_from_env && <p className="text-muted-foreground text-xs">当前凭据来自环境变量；可在此保存新的 Key 或停用数据源。</p>}
      </div>
      {source.id === "fofa" && <div className="grid gap-1.5">
        <Label htmlFor="mapping-fofa-email" className="text-xs">FOFA Email（按账户要求填写）</Label>
        <Input id="mapping-fofa-email" type="email" value={email} onChange={(event) => setEmail(event.target.value)} />
      </div>}
      <details className="min-w-0">
        <summary className="text-muted-foreground cursor-pointer text-xs">连接配置</summary>
        <div className="mt-3 grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor={`mapping-${source.id}-endpoint`} className="text-xs">API 地址</Label>
            <Input id={`mapping-${source.id}-endpoint`} value={endpoint} onChange={(event) => setEndpoint(event.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`mapping-${source.id}-proxy`} className="text-xs">出口代理（留空跟随全局代理）</Label>
            <Input id={`mapping-${source.id}-proxy`} value={proxy} onChange={(event) => setProxy(event.target.value)} placeholder="http://、https:// 或 socks5://" />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`mapping-${source.id}-timeout`} className="text-xs">超时（秒）</Label>
            <Input id={`mapping-${source.id}-timeout`} type="number" min={1} max={120} value={timeout} onChange={(event) => setTimeout(event.target.value)} />
          </div>
        </div>
      </details>
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" variant="outline" onClick={() => save()}><SaveIcon />{saving ? "保存中…" : `保存 ${label}`}</Button>
        {source.key_set && !source.key_from_env && <Button type="button" size="sm" variant="ghost" onClick={() => save(true)}>清除 Key 并停用</Button>}
      </div>
    </fieldset>
  );
}

export function MappingSourcesCard() {
  const [sources, setSources] = React.useState<MappingSource[]>([]);
  const [error, setError] = React.useState("");
  const load = React.useCallback(() => {
    setError("");
    api.mappingSources().then(setSources).catch((error) => setError((error as Error).message));
  }, []);
  React.useEffect(load, [load]);
  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><NetworkIcon className="size-4" />网络测绘数据源</CardTitle>
        <CardDescription>配置后由 Agent 查询已启用的测绘源。API Key 不会返回到对话；此处只管理数据源，不发起查询。</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {error ? <div className="grid gap-2 text-sm"><p className="text-destructive">加载失败：{error}</p><Button size="sm" variant="outline" onClick={load}>重试</Button></div>
          : sources.length === 0 ? <p className="text-muted-foreground text-sm">正在加载…</p>
          : sources.map((source) => <SourceEditor key={source.id} source={source} onSaved={(saved) => setSources((current) => current.map((item) => item.id === saved.id ? saved : item))} />)}
      </CardContent>
    </Card>
  );
}
