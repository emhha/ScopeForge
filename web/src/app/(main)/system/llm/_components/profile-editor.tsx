"use client";
import * as React from "react";

import { ChevronDownIcon, Loader2Icon, PlugZapIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { CatalogReasoning, LLMProfile, LLMRetryOverride } from "@/lib/types";

import { CatalogPicker } from "./catalog-picker";
import { ProfileRetryFields, ZERO_OVERRIDE } from "./retry";

const DEFAULT = "__default__";
const protocols = [
  { value: "openai", label: "OpenAI Chat Completions" },
  { value: "openai-responses", label: "OpenAI Responses" },
  { value: "anthropic", label: "Anthropic Messages" },
];
const selectStyle = "h-9 min-w-0 w-full rounded-md border bg-background px-3 text-sm";
const fieldStyle = "grid min-w-0 gap-2";
const numeric = (value: string) => Math.max(0, Number(value) || 0);

export function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState("openai");
  const [model, setModel] = React.useState("");
  const [baseURL, setBaseURL] = React.useState("");
  const [defaultURL, setDefaultURL] = React.useState("");
  const [key, setKey] = React.useState("");
  const [providerChanged, setProviderChanged] = React.useState(false);
  const [proxy, setProxy] = React.useState("");
  const [cw, setCw] = React.useState("0");
  const [output, setOutput] = React.useState("0");
  const [tokenField, setTokenField] = React.useState("");
  const [thinking, setThinking] = React.useState("");
  const [budget, setBudget] = React.useState("0");
  const [effort, setEffort] = React.useState("");
  const [capabilities, setCapabilities] = React.useState<CatalogReasoning>();
  const [streaming, setStreaming] = React.useState(true);
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [priority, setPriority] = React.useState("0");
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [sessionHeader, setSessionHeader] = React.useState("");
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE);
  const [models, setModels] = React.useState<string[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [error, setError] = React.useState("");
  const [customOpen, setCustomOpen] = React.useState(false);
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setFormat(profile?.format ?? "openai");
    setModel(profile?.model ?? "");
    setBaseURL(profile?.base_url ?? "");
    setDefaultURL("");
    setKey("");
    setProviderChanged(false);
    setProxy(profile?.proxy ?? "");
    setCw(String(profile?.context_window_k ?? 0));
    setOutput(String(profile?.max_tokens ?? 0));
    setTokenField(profile?.max_tokens_field ?? "");
    setThinking(profile?.thinking_type ?? "");
    setBudget(String(profile?.thinking_budget ?? 0));
    setEffort(profile?.reasoning_effort ?? "");
    setCapabilities(undefined);
    setStreaming(profile?.streaming ?? true);
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setSessionHeader(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setModels([]);
    setError("");
    setCustomOpen(false);
  }, [open, profile]);
  const keyHint = providerChanged ? "" : profile?.api_key_hint;
  const baseInvalid = React.useMemo(() => {
    if (!baseURL.trim()) return false;
    try {
      const u = new URL(baseURL.trim());
      return !["https:", "http:"].includes(u.protocol) || !u.hostname || !!u.username || !!u.password;
    } catch {
      return true;
    }
  }, [baseURL]);
  const validate = () => {
    if (!model.trim()) {
      setError("请选择或填写模型 ID。");
      return false;
    }
    if (baseInvalid) {
      setCustomOpen(true);
      setError("接口地址需为有效的 HTTP 或 HTTPS URL。");
      return false;
    }
    if (providerChanged && profile?.api_key_hint && !key.trim()) {
      setError("提供商已切换，请填写对应的 API Key。");
      return false;
    }
    return true;
  };
  async function loadModels() {
    if (loadingModels) return;
    if (!key.trim() && !keyHint) {
      setError("请先填写 API Key，再获取接口模型。");
      return;
    }
    setLoadingModels(true);
    setError("");
    try {
      const result = await api.fetchLLMModels(
        format,
        baseURL,
        key,
        proxy,
        providerChanged ? undefined : profile ? Number(profile.id) : undefined,
      );
      if (!result.ok || !result.models?.length) throw new Error(result.error ?? "接口未返回模型列表");
      setModels(result.models);
    } catch (e) {
      setError(`获取模型失败：${(e as Error).message}。仍可手动填写模型 ID。`);
    } finally {
      setLoadingModels(false);
    }
  }
  async function test() {
    if (!validate() || testing) return;
    if (!key.trim() && !keyHint) {
      setError("请填写 API Key 后测试连接。");
      return;
    }
    setTesting(true);
    setError("");
    try {
      const result = await api.testLLM(
        format,
        model,
        baseURL,
        key,
        proxy,
        thinking,
        effort,
        providerChanged ? undefined : profile ? Number(profile.id) : undefined,
        streaming,
        numeric(budget),
        sessionHeader.trim(),
      );
      if (!result.ok) throw new Error(result.error ?? "模型未正常响应");
      toast.success(`连接成功 · ${result.latency_ms ?? "?"}ms`, { description: result.reply });
    } catch (e) {
      setError(`测试失败：${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }
  async function save() {
    if (!validate() || saving) return;
    setSaving(true);
    setError("");
    try {
      const result = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim() || model.trim(),
        format,
        model: model.trim(),
        base_url: baseURL.trim(),
        api_key: key,
        proxy: proxy.trim(),
        context_window_k: numeric(cw),
        max_tokens: numeric(output),
        max_tokens_field: format === "openai" ? tokenField : "",
        thinking_type: thinking,
        thinking_budget: numeric(budget),
        reasoning_effort: effort,
        streaming,
        rate_per_second: numeric(rps),
        rate_per_minute: numeric(rpm),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        session_header_key: sessionHeader.trim(),
        retry,
      });
      onSaved(String(result.id));
      onOpenChange(false);
      toast.success(profile ? "模型配置已保存" : "模型配置已添加，可在列表中设为激活");
    } catch (e) {
      setError(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="gap-0 p-0 data-[side=right]:w-full data-[side=right]:sm:max-w-xl">
        <SheetHeader className="shrink-0 border-b px-6 py-5 pr-12">
          <SheetTitle className="flex items-center gap-2">
            {profile ? "编辑模型" : "添加模型"}
            {profile?.is_default && <Badge variant="outline">当前激活</Badge>}
          </SheetTitle>
          <SheetDescription>选择提供商与模型填入参数，所有设置都可继续修改。</SheetDescription>
        </SheetHeader>
        <form
          className="flex min-h-0 flex-1 flex-col"
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-6 py-5">
            <CatalogPicker
              key={profile?.id ?? "new"}
              value={{
                model,
                format,
                baseURL,
                thinking,
                effort,
                contextWindowK: numeric(cw),
                maxTokens: numeric(output),
              }}
              onCapabilities={setCapabilities}
              credentials={
                <div className={fieldStyle}>
                  <Label htmlFor="p-api-key">API Key</Label>
                  <Input
                    id="p-api-key"
                    type="password"
                    autoComplete="new-password"
                    value={key}
                    placeholder={keyHint ? `已保存 ${keyHint}，留空保持不变` : "填写提供商的 API Key"}
                    onChange={(e) => setKey(e.target.value)}
                  />
                </div>
              }
              onProvider={(provider, endpoint) => {
                setModels([]);
                setError("");
                if (!provider || !endpoint) {
                  setCustomOpen(true);
                  return;
                }
                setFormat(endpoint.format);
                setBaseURL(endpoint.base_url);
                setDefaultURL(endpoint.base_url);
                setModel("");
                setCw("0");
                setOutput("0");
                setTokenField(endpoint.max_tokens_field ?? "");
                setThinking("");
                setEffort("");
                setBudget("0");
                setProviderChanged(true);
              }}
              onModel={(selected, endpoint) => {
                setName((current) => (!current.trim() || current === model ? selected.id : current));
                if (!baseURL || baseURL === defaultURL) setBaseURL(endpoint.base_url);
                setDefaultURL(endpoint.base_url);
                setFormat(endpoint.format);
                setModel(selected.id);
                setCw(String(Math.floor(selected.context / 1000)));
                setOutput(String(selected.output));
                setTokenField(endpoint.max_tokens_field ?? "");
                setThinking("");
                setEffort("");
                setBudget(String(endpoint.reasoning.budget?.min ?? 0));
                setError("");
              }}
              onModelText={setModel}
              onReasoning={(thinking, effort) => {
                setThinking(thinking);
                setEffort(effort);
              }}
              endpointModels={models}
              loadingModels={loadingModels}
              onLoadModels={() => void loadModels()}
            />
            <details
              className="group rounded-lg border"
              open={customOpen}
              onToggle={(e) => setCustomOpen(e.currentTarget.open)}
            >
              <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-sm font-medium">
                自定义设置
                <ChevronDownIcon className="size-4 shrink-0 transition-transform group-open:rotate-180" />
              </summary>
              <div className="space-y-5 border-t px-4 py-4">
                {field("p-name", "配置名称", name, setName, "text", model || "例如：OpenAI 工作模型")}
                <div className={fieldStyle}>
                  <Label htmlFor="p-base-url">接口地址</Label>
                  <Input
                    id="p-base-url"
                    type="url"
                    value={baseURL}
                    placeholder={format === "anthropic" ? "https://api.anthropic.com" : "https://api.openai.com/v1"}
                    aria-invalid={baseInvalid}
                    onChange={(e) => setBaseURL(e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">
                    可改为中转站或本地服务地址，选择模型会保留此处的自定义地址。
                  </p>
                </div>
                {choice("p-format", "API 协议", format, setFormat, protocols)}
                <div className="grid gap-4 sm:grid-cols-2">
                  {field("p-cw", "上下文窗口（K tokens）", cw, setCw, "number")}
                  {field("p-max-tokens", "最大输出（tokens）", output, setOutput, "number")}
                </div>
                <p className="text-xs text-muted-foreground">
                  目录值已填入，可按实际限制调整。上下文填 0 使用 200K；最大输出填 0 使用服务默认。
                </p>
                {format === "openai" &&
                  choice("p-token-field", "输出上限字段", tokenField, setTokenField, [
                    { value: "", label: "max_tokens" },
                    { value: "max_completion_tokens", label: "max_completion_tokens" },
                  ])}
                {!(capabilities?.supported && (capabilities.toggle || capabilities.budget)) &&
                  choice(
                    "p-thinking",
                    "思考模式（手动指定）",
                    thinking || DEFAULT,
                    (v) => setThinking(v === DEFAULT ? "" : v),
                    [
                      { value: DEFAULT, label: "模型默认（不发送参数）" },
                      { value: "disabled", label: "关闭" },
                      { value: "enabled", label: "开启" },
                      ...(format === "anthropic" ? [{ value: "adaptive", label: "自适应" }] : []),
                    ],
                  )}
                {!capabilities?.effort?.length &&
                  choice(
                    "p-effort",
                    "思考强度（手动指定）",
                    effort || DEFAULT,
                    (v) => setEffort(v === DEFAULT ? "" : v),
                    [
                      { value: DEFAULT, label: "模型默认（不发送参数）" },
                      ...["none", "minimal", "low", "medium", "high", "xhigh", "max"].map((v) => ({
                        value: v,
                        label: v,
                      })),
                    ],
                  )}
                {format === "anthropic" && (
                  <div className="space-y-2">
                    {field("p-thinking-budget", "固定思考预算（tokens）", budget, setBudget, "number")}
                    <p className="text-xs text-muted-foreground">
                      0 使用默认 2048，需小于最大输出；自适应模式不使用固定预算。
                    </p>
                  </div>
                )}
                <div className="flex items-center justify-between gap-4">
                  <Label htmlFor="p-stream">流式输出</Label>
                  <Switch id="p-stream" checked={streaming} onCheckedChange={setStreaming} />
                </div>
                {field("p-proxy", "模型请求代理", proxy, setProxy, "text", "留空直连，或填写 http:// / socks5:// 地址")}
                <div className="grid gap-4 sm:grid-cols-2">
                  {field("p-rps", "每秒请求上限", rps, setRps, "number")}
                  {field("p-rpm", "每分钟请求上限", rpm, setRpm, "number")}
                </div>
                <p className="text-xs text-muted-foreground">限速由全部 Agent 共享，0 表示不限。</p>
                {field("p-priority", "轮询优先级", priority, setPriority, "number")}
                <div className="flex items-center justify-between gap-4">
                  <Label htmlFor="p-pool-exclude">不参与故障转移轮询</Label>
                  <Switch id="p-pool-exclude" checked={poolExclude} onCheckedChange={setPoolExclude} />
                </div>
                {field(
                  "p-session-header",
                  "会话标识请求头",
                  sessionHeader,
                  setSessionHeader,
                  "text",
                  "可选，例如 X-Session-ID",
                )}
                <ProfileRetryFields value={retry} onChange={setRetry} />
              </div>
            </details>
            {!customOpen && (
              <p className="break-all text-xs text-muted-foreground">
                {protocols.find((p) => p.value === format)?.label} · {baseURL || "使用协议默认地址"}
              </p>
            )}
            {error && (
              <p
                role="alert"
                className="rounded-md border border-destructive/20 bg-destructive/5 p-3 text-sm text-destructive"
              >
                {error}
              </p>
            )}
          </div>
          <div className="flex shrink-0 items-center justify-between gap-3 border-t px-6 py-4">
            <Button type="button" variant="outline" disabled={testing || saving} onClick={() => void test()}>
              {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}测试连接
            </Button>
            <div className="flex gap-2">
              <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
                取消
              </Button>
              <Button type="submit" disabled={saving || testing}>
                {saving && <Loader2Icon className="animate-spin" />}
                {profile ? "保存" : "添加模型"}
              </Button>
            </div>
          </div>
        </form>
      </SheetContent>
    </Sheet>
  );
}

function field(
  id: string,
  label: string,
  value: string,
  onChange: (value: string) => void,
  type: "text" | "number" = "text",
  placeholder?: string,
) {
  return (
    <div className={fieldStyle}>
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} type={type} value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />
    </div>
  );
}
function choice(
  id: string,
  label: string,
  value: string,
  onChange: (value: string) => void,
  options: { value: string; label: string }[],
) {
  return (
    <div className={fieldStyle}>
      <Label htmlFor={id}>{label}</Label>
      <select id={id} className={selectStyle} value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  );
}
