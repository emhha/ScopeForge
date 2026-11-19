import type { TaskLLMResolution } from "@/lib/types";

export const MODEL_SOURCE_LABELS: Record<TaskLLMResolution["source"], string> = {
  agent_binding: "角色绑定",
  conversation: "会话指定",
  task_chain: "任务配置链",
  global_profile: "全局激活",
  environment: "环境配置",
  global: "全局配置",
};

export function modelResolutionLabel(model: TaskLLMResolution): string {
  if (!model.available) return model.reason ?? "模型不可用";
  return `预计下次：${model.model} · ${MODEL_SOURCE_LABELS[model.source]}${model.fallback_allowed ? " · 可故障转移" : ""}`;
}
