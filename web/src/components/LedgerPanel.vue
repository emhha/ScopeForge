<script setup lang="ts">
// 漏洞账本视图(phase2/02 §1):只增不改,回执状态推进。
import { computed } from 'vue'
import { useBoardStore } from '../stores/board'
import { fmtTime, ledgerStatusMeta, severityMeta } from '../utils/format'

const emit = defineEmits<{ (e: 'open-evidence', ref: string): void }>()
const board = useBoardStore()

const stats = computed(() => board.ledgerStats)
const rows = computed(() => [...board.vulnerabilities].sort((a, b) => (b.submitted_at || 0) - (a.submitted_at || 0)))

function sevCls(s: string) {
  return severityMeta[s]?.cls || 'faint'
}
</script>

<template>
  <div>
    <div class="grid cards mb16" style="grid-template-columns: repeat(auto-fit, minmax(130px, 1fr))">
      <div class="card stat-card">
        <div class="k">已确认(accepted)</div>
        <div class="v text-ok">{{ stats.accepted }}</div>
        <div class="s">成果口径,调度不终止</div>
      </div>
      <div class="card stat-card">
        <div class="k">待回执(submitted)</div>
        <div class="v text-warn">{{ stats.submitted }}</div>
        <div class="s">平台确认前不计成果</div>
      </div>
      <div class="card stat-card">
        <div class="k">重复(duplicate)</div>
        <div class="v">{{ stats.duplicate }}</div>
        <div class="s">结构化幂等/平台回执</div>
      </div>
      <div class="card stat-card">
        <div class="k">误报(FP)</div>
        <div class="v text-danger">{{ stats.false_positive }}</div>
        <div class="s">fpr = FP / 提交总数</div>
      </div>
      <div class="card stat-card">
        <div class="k">驳回(rejected)</div>
        <div class="v">{{ stats.rejected }}</div>
        <div class="s">该方向可重试</div>
      </div>
    </div>

    <div class="panel">
      <div class="panel-title">
        漏洞账本
        <span class="grow"></span>
        <span class="faint mono">{{ stats.total }} 条记录</span>
      </div>

      <div v-if="rows.length === 0" class="empty">
        <div class="big">🚩</div>
        暂无漏洞提交 — 发现漏洞后账本只增不改。
      </div>

      <div v-else class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>状态</th>
              <th>标题</th>
              <th>严重度</th>
              <th>CWE</th>
              <th>资产 / 端点</th>
              <th>证据</th>
              <th>平台回执</th>
              <th>提交时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="v in rows" :key="v.id">
              <td>
                <span class="badge" :class="(ledgerStatusMeta[v.status] || {}).cls">
                  {{ (ledgerStatusMeta[v.status] || {}).icon }} {{ (ledgerStatusMeta[v.status] || {}).label || v.status }}
                </span>
              </td>
              <td>
                <div>{{ v.title || '-' }}</div>
                <div v-if="v.description" class="muted mt4">{{ v.description }}</div>
              </td>
              <td><span class="badge" :class="sevCls(v.severity)">{{ severityMeta[v.severity]?.label || v.severity || 'info' }}</span></td>
              <td class="mono">{{ v.cwe || '-' }}</td>
              <td class="mono">{{ v.asset }}<span v-if="v.endpoint"> @ {{ v.endpoint }}</span></td>
              <td>
                <button v-if="v.evidence_ref" class="btn xs" @click="emit('open-evidence', v.evidence_ref)">证据 →</button>
                <span v-else class="faint">-</span>
              </td>
              <td class="mono">{{ v.platform_ref || '-' }}</td>
              <td class="muted mono nowrap">{{ fmtTime(v.submitted_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
