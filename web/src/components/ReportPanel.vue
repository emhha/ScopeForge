<script setup lang="ts">
// 报告与复核视图(phase2/05):服务端报告事件 + 当前数据即时预览。
// 后端报告落盘在 workdir/reports,前端以 report 事件审计链为准;
// 这里同时用账本/覆盖矩阵/事实生成一份可读预览,便于交付前检查。
import { computed } from 'vue'
import { useBoardStore } from '../stores/board'
import { useTaskEvents } from '../composables/useTaskEvents'
import { fmtTime, fmtUsd, severityMeta, ledgerStatusMeta, cellStatusMeta } from '../utils/format'
import { evSummary } from '../utils/events'

const props = defineProps<{ challenge: string }>()
const board = useBoardStore()
const { events: taskEvents } = useTaskEvents(() => props.challenge, () => null, 2000)

const reportEvents = computed(() =>
  taskEvents.value.filter((e) => e.Kind === 'report' || e.Kind === 'review').reverse(),
)

const vulnerabilities = computed(() => [...board.vulnerabilities].sort((a, b) => (b.submitted_at || 0) - (a.submitted_at || 0)))
const accepted = computed(() => vulnerabilities.value.filter((v) => v.status === 'accepted'))

const sevDistribution = computed(() => {
  const counts: Record<string, number> = {}
  for (const v of accepted.value) counts[v.severity] = (counts[v.severity] || 0) + 1
  return counts
})

const coverageSummary = computed(() => {
  const total = board.cells.length
  const confirmed = board.cells.filter((c) => c.status === 'confirmed').length
  const excluded = board.cells.filter((c) => c.status === 'dead' || c.status === 'skipped').length
  return { total, confirmed, open: total - confirmed - excluded, excluded }
})

const remainingRisks = computed(() => [
  ...board.facts.filter((f) => f.prefix === 'hyp:' && f.state === 'candidate').map((f) => f.text),
  ...board.cells.filter((c) => c.status === 'skipped').map((c) => `${c.cwe || 'CWE'} @ ${c.asset}${c.endpoint} — ${c.skip_reason || '已跳过'}`),
])
</script>

<template>
  <div>
    <div class="panel">
      <div class="panel-title">
        📄 服务端报告事件
        <span class="grow"></span>
        <span class="faint">生成 / 脱敏 / 复核审计链来自事件流</span>
      </div>
      <div v-if="reportEvents.length === 0" class="empty">任务终止后自动生成报告;此处将显示 report / review 事件。</div>
      <div v-else class="timeline">
        <div v-for="e in reportEvents" :key="e.Seq" class="tl-item">
          <div class="head">
            <span class="badge" :class="e.Kind === 'report' ? 'purple' : 'warn'">{{ e.Kind }}</span>
            <span class="mono faint">seq {{ e.Seq }} · {{ fmtTime(e.TS) }}</span>
          </div>
          <div class="sum mt4">{{ evSummary(e.Kind, e.Payload) }}</div>
          <div v-if="e.Payload?.path" class="mono muted mt4">文件: {{ e.Payload.path }}</div>
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="panel-title">报告预览(前端根据账本/覆盖矩阵/事实即时生成,最终以服务端导出为准)</div>

      <div class="grid cards mb16" style="grid-template-columns: repeat(auto-fit, minmax(140px, 1fr))">
        <div class="card stat-card">
          <div class="k">已确认漏洞</div>
          <div class="v text-ok">{{ accepted.length }}</div>
          <div class="s">{{ Object.entries(sevDistribution).map(([k, v]) => `${severityMeta[k]?.label || k}×${v}`).join(' / ') || '无' }}</div>
        </div>
        <div class="card stat-card">
          <div class="k">覆盖矩阵</div>
          <div class="v">{{ coverageSummary.total }}</div>
          <div class="s">已覆盖 {{ coverageSummary.confirmed }} · 未覆盖 {{ coverageSummary.open }} · 排除 {{ coverageSummary.excluded }}</div>
        </div>
        <div class="card stat-card">
          <div class="k">成本</div>
          <div class="v">{{ fmtUsd(board.spend?.CostUSD) }}</div>
          <div class="s">turns {{ board.spend?.Turns || 0 }}</div>
        </div>
      </div>

      <h4 class="mt16 mb8">漏洞清单</h4>
      <div v-if="vulnerabilities.length === 0" class="empty">暂无账本记录</div>
      <div v-else class="table-wrap">
        <table>
          <thead>
            <tr><th>状态</th><th>标题</th><th>严重度</th><th>CWE</th><th>资产</th><th>端点</th><th>证据</th></tr>
          </thead>
          <tbody>
            <tr v-for="v in vulnerabilities" :key="v.id">
              <td><span class="badge" :class="(ledgerStatusMeta[v.status] || {}).cls">{{ (ledgerStatusMeta[v.status] || {}).label || v.status }}</span></td>
              <td>{{ v.title || '-' }}</td>
              <td><span class="badge" :class="severityMeta[v.severity]?.cls">{{ severityMeta[v.severity]?.label || v.severity }}</span></td>
              <td class="mono">{{ v.cwe || '-' }}</td>
              <td class="mono">{{ v.asset }}</td>
              <td class="mono">{{ v.endpoint || '-' }}</td>
              <td class="mono">{{ v.evidence_ref || '-' }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <h4 class="mt16 mb8">覆盖矩阵摘要</h4>
      <div class="legend">
        <span v-for="(meta, status) in cellStatusMeta" :key="status" class="item">
          <span class="badge" :class="meta.cls">{{ meta.label }}</span>
          {{ board.cells.filter((c) => c.status === status).length }}
        </span>
      </div>

      <h4 class="mt16 mb8">剩余风险 / 待验证假设</h4>
      <div v-if="remainingRisks.length === 0" class="empty">无</div>
      <ul v-else class="muted">
        <li v-for="(risk, i) in remainingRisks.slice(0, 30)" :key="i">{{ risk }}</li>
      </ul>
    </div>
  </div>
</template>
