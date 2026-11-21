"use client";

import * as React from "react";

import { ChevronDownIcon, Loader2Icon, RefreshCwIcon, SearchIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { api } from "@/lib/api";
import type { CatalogAPI, CatalogModel, CatalogProvider, CatalogReasoning, ModelCatalog } from "@/lib/types";

const CUSTOM = "custom";
const DEFAULT = "__default__";
const common = ["openai", "anthropic", "deepseek", "google", "alibaba", "moonshotai", "zai"];
const control = "h-9 w-full min-w-0 rounded-md border bg-background px-3 text-sm";

// The catalog supplies defaults to the same draft that the custom fields edit.
// Refreshing metadata never writes back into an open draft.
export function CatalogPicker({
  value,
  credentials,
  onProvider,
  onModel,
  onModelText,
  onReasoning,
  onCapabilities,
  endpointModels,
  loadingModels,
  onLoadModels,
}: {
  value: {
    model: string;
    format: string;
    baseURL: string;
    thinking: string;
    effort: string;
    contextWindowK: number;
    maxTokens: number;
  };
  credentials: React.ReactNode;
  onProvider: (provider: CatalogProvider | undefined, api: CatalogAPI | undefined) => void;
  onModel: (model: CatalogModel, api: CatalogAPI) => void;
  onModelText: (model: string) => void;
  onReasoning: (thinking: string, effort: string) => void;
  onCapabilities: (capabilities: CatalogReasoning | undefined) => void;
  endpointModels: string[];
  loadingModels: boolean;
  onLoadModels: () => void;
}) {
  const [catalog, setCatalog] = React.useState<ModelCatalog>();
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [providerID, setProviderID] = React.useState<string>();
  const [providerQuery, setProviderQuery] = React.useState("");
  const [modelQuery, setModelQuery] = React.useState("");
  const [providerOpen, setProviderOpen] = React.useState(false);
  const [modelOpen, setModelOpen] = React.useState(false);
  const load = React.useCallback(async (refresh = false) => {
    setLoading(true);
    setError("");
    try {
      setCatalog(await api.modelCatalog(refresh));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);
  React.useEffect(() => {
    if (endpointModels.length) {
      setModelQuery("");
      setModelOpen(true);
    }
  }, [endpointModels]);
  const providers = React.useMemo(
    () =>
      [...(catalog?.providers ?? [])].sort((a, b) => {
        const rank = (id: string) => (common.includes(id) ? common.indexOf(id) : common.length);
        return rank(a.id) - rank(b.id) || a.name.localeCompare(b.name);
      }),
    [catalog],
  );
  const inferred = providers.find((p) =>
    p.models.some(
      (m) =>
        m.id === value.model &&
        m.apis.some(
          (a) => a.format === value.format && a.base_url.replace(/\/$/, "") === value.baseURL.replace(/\/$/, ""),
        ),
    ),
  )?.id;
  const selected = providerID ?? inferred ?? CUSTOM;
  const provider = providers.find((p) => p.id === selected);
  const model =
    provider?.models.find((m) => m.id === value.model) ??
    providers
      .flatMap((p) => p.models)
      .find((m) => m.id === value.model && m.apis.some((a) => a.format === value.format));
  const endpoint = model?.apis.find((a) => a.format === value.format);
  const reasoning = endpoint?.reasoning;
  React.useEffect(() => {
    onCapabilities(reasoning);
  }, [reasoning, onCapabilities]);
  const candidates = React.useMemo(() => {
    const result = new Map<string, { id: string; name: string; model?: CatalogModel; api?: CatalogAPI }>();
    for (const id of endpointModels) result.set(id, { id, name: "当前接口" });
    for (const p of provider ? [provider] : providers)
      for (const m of p.models) {
        const a = m.apis.find((a) => a.format === value.format) ?? m.apis[0];
        const old = result.get(m.id);
        if (!old?.model) result.set(m.id, { id: m.id, name: `${m.name} · ${p.name}`, model: m, api: a });
      }
    return [...result.values()];
  }, [endpointModels, provider, providers, value.format]);
  const matches = candidates.filter((m) => `${m.id} ${m.name}`.toLowerCase().includes(modelQuery.toLowerCase()));
  return (
    <>
      <div className="grid min-w-0 gap-2">
        <div className="flex items-center justify-between gap-2">
          <Label htmlFor="catalog-provider">提供商</Label>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            className="h-7 text-xs text-muted-foreground"
            disabled={loading}
            onClick={() => void load(true)}
          >
            {loading ? <Loader2Icon className="size-3 animate-spin" /> : <RefreshCwIcon className="size-3" />} 在线更新
          </Button>
        </div>
        <Popover open={providerOpen} onOpenChange={setProviderOpen} modal>
          <PopoverTrigger asChild>
            <Button
              id="catalog-provider"
              type="button"
              variant="outline"
              className="w-full min-w-0 justify-between font-normal"
            >
              <span className="truncate">{provider?.name ?? "自定义 / 兼容接口"}</span>
              <ChevronDownIcon className="size-4 shrink-0" />
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-[min(440px,calc(100vw-2rem))] p-2">
            <Input
              aria-label="搜索提供商"
              placeholder="搜索提供商，例如 OpenAI、DeepSeek"
              value={providerQuery}
              onChange={(e) => setProviderQuery(e.target.value)}
            />
            <div className="mt-2 max-h-64 overflow-y-auto overscroll-contain">
              <button
                type="button"
                className="w-full rounded-md px-3 py-2 text-left text-sm hover:bg-accent"
                onClick={() => {
                  setProviderID(CUSTOM);
                  onProvider(undefined, undefined);
                  setProviderOpen(false);
                }}
              >
                自定义 / 兼容接口
              </button>
              {providers
                .filter((p) => `${p.name} ${p.id}`.toLowerCase().includes(providerQuery.toLowerCase()))
                .map((p) => (
                  <button
                    type="button"
                    key={p.id}
                    className="flex w-full items-center justify-between gap-3 rounded-md px-3 py-2 text-left text-sm hover:bg-accent"
                    onClick={() => {
                      setProviderID(p.id);
                      setProviderOpen(false);
                      setModelQuery("");
                      onProvider(p, p.models[0]?.apis[0]);
                    }}
                  >
                    <span>{p.name}</span>
                    <span className="truncate text-xs text-muted-foreground">{p.id}</span>
                  </button>
                ))}
            </div>
          </PopoverContent>
        </Popover>
        {error ? (
          <p role="status" className="break-words text-xs text-amber-600">
            {error}。{catalog ? "继续使用当前目录。" : "仍可手动填写模型和自定义设置。"}
            <button type="button" onClick={() => void load(true)} className="ml-1 underline">
              重试
            </button>
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">
            {loading
              ? "正在加载模型目录…"
              : catalog
                ? `${catalog.origin === "bundled" ? "内置目录" : "在线目录"} · ${new Date(catalog.updated_at).toLocaleDateString()}，可直接选择或手动填写。`
                : "选择提供商填入默认参数，也可以直接填写自己的接口。"}
          </p>
        )}
      </div>
      {credentials}
      <div className="grid min-w-0 gap-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Label htmlFor="p-model">模型 ID</Label>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            className="h-7 text-xs text-muted-foreground"
            onClick={onLoadModels}
            disabled={loadingModels || !value.baseURL}
          >
            {loadingModels && <Loader2Icon className="size-3 animate-spin" />}获取接口模型
          </Button>
        </div>
        <div className="flex min-w-0 gap-2">
          <Input
            id="p-model"
            className="min-w-0 flex-1 font-mono"
            value={value.model}
            placeholder="选择或输入模型 ID"
            onChange={(e) => onModelText(e.target.value)}
          />
          <Popover open={modelOpen} onOpenChange={setModelOpen} modal>
            <PopoverTrigger asChild>
              <Button
                type="button"
                variant="outline"
                size="icon"
                aria-label="选择目录模型"
                className="shrink-0"
                disabled={!candidates.length}
              >
                <ChevronDownIcon />
              </Button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-[min(440px,calc(100vw-2rem))] p-2">
              <div className="relative">
                <SearchIcon className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
                <Input
                  className="pl-8"
                  aria-label="搜索目录模型"
                  placeholder="搜索模型 ID 或名称"
                  value={modelQuery}
                  onChange={(e) => setModelQuery(e.target.value)}
                />
              </div>
              <div className="mt-2 max-h-64 overflow-y-auto overscroll-contain">
                {matches.slice(0, 100).map((m) => (
                  <button
                    type="button"
                    key={m.id}
                    className="flex w-full min-w-0 flex-col gap-1 rounded-md px-3 py-2 text-left hover:bg-accent"
                    onClick={() => {
                      if (m.model && m.api) onModel(m.model, m.api);
                      else onModelText(m.id);
                      setModelOpen(false);
                    }}
                  >
                    <span className="break-all font-mono text-sm">{m.id}</span>
                    <span className="text-xs text-muted-foreground">{m.name}</span>
                  </button>
                ))}
                {!matches.length && (
                  <p className="p-3 text-xs text-muted-foreground">没有匹配项，可在表单中直接输入模型 ID。</p>
                )}
                {matches.length > 100 && (
                  <p className="p-3 text-xs text-muted-foreground">共 {matches.length} 个模型，请搜索缩小范围。</p>
                )}
              </div>
            </PopoverContent>
          </Popover>
        </div>
        {value.model && (
          <p className="flex flex-wrap gap-x-2 text-xs text-muted-foreground">
            <span>
              上下文 {value.contextWindowK ? `${value.contextWindowK}K` : "默认 200K"} · 最大输出{" "}
              {value.maxTokens ? `${value.maxTokens.toLocaleString()} tokens` : "服务默认"}
            </span>
            {model &&
              (value.contextWindowK !== Math.floor(model.context / 1000) || value.maxTokens !== model.output) && (
                <span className="text-primary">已自定义</span>
              )}
          </p>
        )}
      </div>
      {reasoning?.supported && (reasoning.toggle || reasoning.budget || !!reasoning.effort?.length) && (
        <div className="grid gap-3 sm:grid-cols-2">
          {(reasoning.toggle || reasoning.budget) && (
            <div className="grid gap-2">
              <Label htmlFor="p-quick-thinking">思考模式</Label>
              <select
                id="p-quick-thinking"
                className={control}
                value={value.thinking || DEFAULT}
                onChange={(e) => onReasoning(e.target.value === DEFAULT ? "" : e.target.value, value.effort)}
              >
                <option value={DEFAULT}>模型默认</option>
                <option value="disabled">关闭</option>
                <option value="enabled">开启</option>
                {value.format === "anthropic" && !!reasoning.effort?.length && <option value="adaptive">自适应</option>}
              </select>
            </div>
          )}
          {!!reasoning.effort?.length && (
            <div className="grid gap-2">
              <Label htmlFor="p-quick-effort">思考强度</Label>
              <select
                id="p-quick-effort"
                className={control}
                value={value.effort || DEFAULT}
                onChange={(e) => {
                  const effort = e.target.value === DEFAULT ? "" : e.target.value;
                  onReasoning(value.format === "anthropic" && effort ? "adaptive" : value.thinking, effort);
                }}
              >
                <option value={DEFAULT}>模型默认</option>
                {reasoning.effort.map((v) => (
                  <option key={v} value={v}>
                    {v === "none" ? "none（不思考）" : v}
                  </option>
                ))}
              </select>
            </div>
          )}
        </div>
      )}
    </>
  );
}
