<script setup lang="ts">
// 任务发起弹窗:phase2 入口 = 任务描述注入(04 §1)。
// 三种描述模板只是帮助构造 Task 文本;platform_url 会作为聚焦目标注入。
import { ref } from 'vue'
import { api } from '../api'

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'started', runId: string): void
}>()

type TemplateType = 'focused' | 'src' | 'breach'

const taskType = ref<TemplateType>('src')
const targetUrl = ref('')
const description = ref('')
const submitting = ref(false)
const error = ref('')

const TEMPLATES: Record<TemplateType, string> = {
  src: `## 任务
对 {TARGET} 进行授权 Web 安全测试,尽可能多地发现中高危漏洞。

## 范围
- 仅限目标站点及其子域;禁止 DoS/压测、拖库、社工、篡改生产数据。

## 分级标准
- 高危:RCE / SQL 注入 / 任意文件读 / 越权(IDOR) / SSRF / 存储型 XSS
- 中危:反射 XSS / CSRF / 源码密钥泄露 / 弱口令
- 低危:点击劫持 / 版本信息泄露

## 输出契约
每条漏洞:标题 / URL+参数 / 复现步骤 / 危害 / 修复建议 / 证据(请求响应)。
最后按严重度分级汇总漏洞清单。`,
  focused: `## 任务
对 {TARGET} 做指定类型 Web 漏洞检测。

## 范围
- 仅限指定端点;禁止扫描其他端口/目录/主机/子域。
- 禁止 DoS/压测、拖库、社工、篡改生产数据。

## 约束
- 只测要求的漏洞类型;确认后即终止,不扩展其他方向。`,
  breach: `## 任务
获取 {TARGET} 相关资产的目标权限。

## 范围
- 目标站点及其关联网络;禁止 DoS/压测、拖库、社工、篡改生产数据。
- 外网入口 → 立足点 → 横向移动 → 目标权限,深度优先。

## 约束
- 获得立足点后优先搭建隧道/代理深入内网;外网其他站点降低优先级。
- 达成目标状态即终止。`,
}

const TYPE_LABEL: Record<TemplateType, { label: string; hint: string }> = {
  focused: { label: '聚焦单点检测', hint: '指定端点 + 漏洞类型,确认即停' },
  src: { label: 'SRC 漏洞挖掘', hint: '全面扫描,尽可能多找漏洞' },
  breach: { label: '企业渗透(Breach)', hint: '获取靶标权限,深度优先' },
}

function fillTemplate() {
  error.value = ''
  if (!targetUrl.value.trim()) {
    error.value = '请先填写目标 URL,再生成模板'
    return
  }
  description.value = TEMPLATES[taskType.value].replaceAll('{TARGET}', targetUrl.value.trim())
}

async function submit() {
  error.value = ''
  const task = description.value.trim()
  if (!task) {
    error.value = '任务描述不能为空'
    return
  }
  if (task.includes('{TARGET}')) {
    error.value = '任务描述包含未替换的占位符 {TARGET}'
    return
  }
  submitting.value = true
  try {
    const res = await api.runTask({
      mode: 'task',
      task,
      platform_url: targetUrl.value.trim() || undefined,
    })
    emit('started', res.run_id)
    emit('close')
  } catch (e: any) {
    error.value = String(e?.message || e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="modal-mask" @click.self="emit('close')">
    <div class="modal">
      <h3 style="margin-top: 0; display: flex; align-items: center; gap: 10px">
        发起任务
        <span class="grow"></span>
        <button class="btn ghost sm" @click="emit('close')">✕</button>
      </h3>

      <div class="muted mb8">任务形态(描述模板)</div>
      <div class="row wrap mb8">
        <button
          v-for="(meta, key) in TYPE_LABEL"
          :key="key"
          class="btn sm"
          :class="{ primary: taskType === key }"
          :title="meta.hint"
          @click="taskType = (key as TemplateType)"
        >
          {{ meta.label }}
        </button>
      </div>
      <div class="muted mb8">{{ TYPE_LABEL[taskType].hint }}</div>

      <div class="row mb8">
        <input
          v-model="targetUrl"
          placeholder="目标 URL,如 http://shop.example.com 或 http://10.0.0.8:3000/login"
          style="flex: 1"
        />
        <button class="btn sm" @click="fillTemplate">生成模板</button>
      </div>
      <textarea
        v-model="description"
        rows="14"
        class="w100 mono"
        spellcheck="false"
        placeholder="任务描述将注入 scout/synthesizer 提示词;范围、分级标准与输出契约写清楚。"
      ></textarea>

      <div v-if="error" class="badge bad mt8">{{ error }}</div>
      <div class="row mt8" style="justify-content: space-between">
        <span class="faint">提交只记账,不终止任务;终止由预算 / 覆盖度收敛 / 穷尽声明决定。</span>
        <div class="row">
          <button class="btn" @click="emit('close')">取消</button>
          <button class="btn primary" :disabled="submitting || !description.trim()" @click="submit">
            {{ submitting ? '启动中…' : '🚀 启动任务' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
