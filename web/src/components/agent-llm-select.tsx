"use client";

import * as React from "react";
import { Link } from "react-router";
import { toast } from "sonner";

import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";

interface ProfileOption {
  id: number | string;
  name: string;
  model: string;
  is_default: boolean;
}

// Save only this agent's model binding; prompt and execution budgets have their own controls.
export function AgentLLMSelect({ agentKey, agentName, profileId, profiles, onSaved }: {
  agentKey: string;
  agentName: string;
  profileId?: number | null;
  profiles: readonly ProfileOption[];
  onSaved: (profileId: number | null) => void;
}) {
  const id = React.useId();
  const [saving, setSaving] = React.useState(false);
  const selected = profileId == null ? "__follow__" : String(profileId);
  const missing = profileId != null && !profiles.some((profile) => String(profile.id) === String(profileId));

  async function change(value: string | null) {
    if (value == null || saving || value === selected) return;
    const next = value === "__follow__" ? null : Number(value);
    setSaving(true);
    try {
      await api.saveAgentConfig(agentKey, { llm_profile_id: next });
      onSaved(next);
      toast.success(`已保存「${agentName}」的 LLM 配置`);
    } catch (error) {
      toast.error("保存 LLM 配置失败：" + (error as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="grid min-w-0 gap-1.5">
      <Label htmlFor={id} className="text-xs">LLM 配置</Label>
      <Select value={selected} onValueChange={change} disabled={saving}>
        <SelectTrigger id={id} className="h-8 w-full min-w-0" aria-label={`${agentName} 的 LLM 配置`}>
          <SelectValue placeholder="选择 LLM 配置" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__follow__">跟随任务 / 会话 / 全局配置</SelectItem>
          {missing && <SelectItem value={String(profileId)} disabled>配置 #{profileId}（不可用）</SelectItem>}
          {profiles.map((profile) => (
            <SelectItem key={profile.id} value={String(profile.id)}>
              {profile.name}（{profile.model}）{profile.is_default ? " · 全局激活" : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {saving && <span className="text-muted-foreground text-xs" role="status">正在保存…</span>}
      {profiles.length === 0 && (
        <span className="text-muted-foreground text-xs">暂无可选配置，请先到 <Link to="/system/llm" className="text-primary underline">LLM 配置</Link> 添加。</span>
      )}
    </div>
  );
}
