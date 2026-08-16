<script setup lang="ts">
// 覆盖矩阵视图(phase2 README 验收底线 4):
// CWE × 资产 × 端点 三维格,状态 = open / claimed / confirmed / dead / skipped。
import { computed, ref } from 'vue'
import { useBoardStore } from '../stores/board'
import type { CoverageCell } from '../api'
import { cellStatusMeta } from '../utils/format'

const emit = defineEmits<{ (e: 'refresh'): void }>()
const board = useBoardStore()
const message = ref('')

const cwes = computed(() => {
  const set = new Set<string>()
  for (const c of board.cells) set.add(c.cwe || '未指定')
  return [...set].sort((a, b) => a.localeCompare(b))
})

interface MatrixRow {
  asset: string
  endpoint: string
  form: string
  cells: Record<string, CoverageCell>
}

const rows = computed<MatrixRow[]>(() => {
  const groups = new Map<string, MatrixRow>()
  for (const cell of board.cells) {
    const key = `${cell.asset}\u0000${cell.endpoint || ''}`
    let row = groups.get(key)
    if (!row) {
      row = { asset: cell.asset, endpoint: cell.endpoint || '/', form: cell.form || '', cells: {} }
      groups.set(key, row)
    }
    row.cells[cell.cwe || '未指定'] = cell
  }
  return [...groups.values()].sort((a, b) => a.asset.localeCompare(b.asset) || a.endpoint.localeCompare(b.endpoint))
})

function countFor(status: string): number {
  return (board.cellCounts as unknown as Record<string, number>)[status] || 0
}

function cellOf(row: MatrixRow, cwe: string): CoverageCell | undefined {
  return row.cells[cwe]
}

async function act(cell: CoverageCell, action: 'skip' | 'dead' | 'reopen') {
  message.value = ''
  const reason = action === 'reopen' ? '' : window.prompt(action === 'skip' ? '跳过原因:' : '排除原因:')
  try {
    await board.setCellState(cell, action, reason || undefined)
    emit('refresh')
  } catch (e: any) {
    message.value = String(e?.message || e)
  }
}
</script>

<template>
  <div>
    <div v-if="message" class="badge bad mb8">{{ message }}</div>

    <div class="panel">
      <div class="panel-title">
        <span>覆盖矩阵</span>
        <span class="grow"></span>
        <span class="badge hyp">{{ board.cellCounts.open }} 未覆盖</span>
        <span class="badge ok">{{ board.cellCounts.claimed + board.cellCounts.confirmed }} 覆盖/进行中</span>
        <span class="badge dead">{{ board.cellCounts.dead + board.cellCounts.skipped }} 排除/跳过</span>
        <button class="btn sm ghost" :disabled="board.loading" @click="board.refresh()">刷新</button>
      </div>

      <div class="legend mb8">
        <span v-for="(meta, status) in cellStatusMeta" :key="status" class="item">
          <span class="badge" :class="meta.cls">{{ meta.icon }} {{ meta.label }}</span>
          {{ countFor(status) }}
        </span>
      </div>

      <div v-if="board.cells.length === 0" class="empty">
        <div class="big">▦</div>
        <div>暂无覆盖矩阵数据 — 等待 Scout 输出 attack_surface,或漏洞确认时自动补格。</div>
        <div v-if="board.vulnerabilities.length > 0" class="muted mt8">
          账本已有漏洞记录但矩阵为空:旧任务数据需重启 serve 触发覆盖矩阵回填迁移。
        </div>
      </div>

      <div v-else class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>资产</th>
              <th>端点</th>
              <th>形态</th>
              <th v-for="cwe in cwes" :key="cwe" class="mono">{{ cwe }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in rows" :key="row.asset + '|' + row.endpoint">
              <td class="mono">{{ row.asset }}</td>
              <td class="mono">{{ row.endpoint }}</td>
              <td class="muted">{{ row.form || '-' }}</td>
              <td v-for="cwe in cwes" :key="cwe">
                <template v-if="cellOf(row, cwe)">
                  <span class="cov-cell" :class="`cov-${cellOf(row, cwe)!.status}`">
                    {{ cellStatusMeta[cellOf(row, cwe)!.status]?.icon || '•' }}
                    {{ cellStatusMeta[cellOf(row, cwe)!.status]?.label || cellOf(row, cwe)!.status }}
                  </span>
                  <div class="row mt4" style="gap: 4px">
                    <button
                      v-if="cellOf(row, cwe)!.status === 'open' || cellOf(row, cwe)!.status === 'claimed'"
                      class="btn xs"
                      @click="act(cellOf(row, cwe)!, 'skip')"
                    >跳过</button>
                    <button
                      v-if="cellOf(row, cwe)!.status === 'open' || cellOf(row, cwe)!.status === 'claimed'"
                      class="btn xs"
                      @click="act(cellOf(row, cwe)!, 'dead')"
                    >排除</button>
                    <button
                      v-if="cellOf(row, cwe)!.status === 'skipped' || cellOf(row, cwe)!.status === 'dead'"
                      class="btn xs"
                      @click="act(cellOf(row, cwe)!, 'reopen')"
                    >重开</button>
                  </div>
                  <div v-if="cellOf(row, cwe)!.skip_reason" class="faint mt4" :title="cellOf(row, cwe)!.skip_reason">
                    {{ cellOf(row, cwe)!.skip_reason }}
                  </div>
                </template>
                <span v-else class="faint">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
