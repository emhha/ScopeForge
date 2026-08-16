<script setup lang="ts">
// 攻击时间线:SSE 增量 + REST 向前翻页,流式增量合并为可读条目,
// 支持 agent(session)过滤与 kind 分组过滤。
import { computed, nextTick, ref, watch } from 'vue'
import { api, type EventItem } from '../api'
import { useBoardStore } from '../stores/board'
import { useTaskEvents } from '../composables/useTaskEvents'
import { eventDetail, kindOf, mergeTimeline, phaseLabel, toolDetail } from '../utils/events'
import { fmtClock, shortId } from '../utils/format'

const props = defineProps<{ challenge: string; agent: string | null }>()
const emit = defineEmits<{
  (e: 'clear-agent'): void
  (e: 'select-session', id: string | null): void
}>()

const board = useBoardStore()
const { events: busEvents } = useTaskEvents(() => props.challenge, () => props.agent, 800)

const extra = ref<EventItem[]>([])
const nextBefore = ref(Number.MAX_SAFE_INTEGER)
const hasMore = ref(true)
const loadingOlder = ref(false)
const error = ref('')
const expanded = ref<string | null>(null)
const kindFilter = ref<'all' | 'key' | 'think' | 'tool' | 'orchestra' | 'security'>('all')
const sessionMeta = computed<Record<string, { label: string; cls: string }>>(() => {
  const map: Record<string, { label: string; cls: string }> = {}
  for (const w of board.workers) {
    const ph = phaseLabel(w.phase)
    map[w.id] = {
      label: w.worker_type === 'synthesizer' ? 'Synthesizer' : ph || w.worker_type,
      cls: w.worker_type === 'synthesizer' ? 'purple' : ph === 'executor' ? 'ok' : ph === 'analyst' ? 'purple' : 'info',
    }
  }
  map['observer'] = { label: 'Observer', cls: 'purple' }
  return map
})

function sessionLabel(id: string): string {
  const meta = sessionMeta.value[id]
  return meta ? `${meta.label} ${shortId(id, 8)}` : shortId(id, 10)
}

function sessionCls(id: string): string {
  return sessionMeta.value[id]?.cls || 'faint'
}

function onSessionSelect(value: string) {
  emit('select-session', value || null)
}

const timelineEl = ref<HTMLDivElement | null>(null)
const stick = ref(true)

const FILTER_GROUPS: Record<Exclude<typeof kindFilter.value, 'all'>, string[]> = {
  key: ['run_started', 'run_done', 'termination', 'finding', 'submission', 'checkpoint', 'board_change', 'report', 'review', 'error'],
  think: ['turn_start', 'reasoning_delta', 'text_delta', 'usage'],
  tool: ['tool_call_start', 'tool_call_args_delta', 'tool_call_result', 'tool_unlock', 'kb_search'],
  orchestra: ['worker_launch', 'worker_done', 'worker_abort', 'scheduler_tick'],
  security: ['denied', 'traffic', 'route', 'listener', 'approval'],
}

const KIND_LABEL: Record<string, string> = {
  all: '全部事件',
  key: '关键 / 结果',
  think: '思考 / 对话',
  tool: '工具',
  orchestra: '编排',
  security: '安全 / 能力',
}

function matchesChallenge(e: EventItem): boolean {
  return e.ChallengeID === props.challenge || (!e.ChallengeID && !!props.agent)
}

function matchingSession(e: EventItem): boolean {
  return !props.agent || e.SessionID === props.agent
}

const combined = computed<EventItem[]>(() => {
  const map = new Map<number, EventItem>()
  for (const e of extra.value) if (matchesChallenge(e) && matchingSession(e)) map.set(e.Seq, e)
  for (const e of busEvents.value) map.set(e.Seq, e)
  return [...map.values()].sort((a, b) => a.Seq - b.Seq)
})

const oldestSeq = computed(() => (combined.value.length ? combined.value[0].Seq : 0))

const merged = computed(() => mergeTimeline(combined.value))

const filtered = computed(() => {
  if (kindFilter.value === 'all') return merged.value
  const kinds = FILTER_GROUPS[kindFilter.value]
  return merged.value.filter((m) => kinds.includes(m.kind))
})

const visible = computed(() => filtered.value.slice(-200))

async function loadOlder() {
  if (loadingOlder.value || !hasMore.value) return
  loadingOlder.value = true
  error.value = ''
  try {
    const before = nextBefore.value
    const session = props.agent || ''
    const page = await api.eventsBeforeMerged(before, 1000, session)
    const events = page.events || []
    if (!events.length) {
      hasMore.value = false
      return
    }
    for (const e of events) extra.value.push(e)
    const rawCount = page.raw_count ?? events.length
    if (rawCount < 1000) {
      nextBefore.value = 0
      hasMore.value = false
    } else {
      const min = Math.min(...events.map((e) => e.Seq))
      nextBefore.value = min
      hasMore.value = min > 0
    }
  } catch (e: any) {
    error.value = String(e?.message || e)
  } finally {
    loadingOlder.value = false
  }
}

/** 初始化 / 切换任务或 agent:清空旧页游标,已有桶中的历史作为最旧边界。 */
watch(
  () => [props.challenge, props.agent] as const,
  async () => {
    extra.value = []
    expanded.value = null
    error.value = ''
    hasMore.value = true
    const existing = busEvents.value
    nextBefore.value = existing.length ? existing[0].Seq : Number.MAX_SAFE_INTEGER
    if (!existing.length) await loadOlder()
  },
  { immediate: true },
)

watch(visible, async () => {
  await nextTick()
  if (stick.value && timelineEl.value) timelineEl.value.scrollTop = timelineEl.value.scrollHeight
})

function onScroll() {
  if (!timelineEl.value) return
  const el = timelineEl.value
  stick.value = el.scrollHeight - el.scrollTop - el.clientHeight < 80
}

function toggle(item: { key: string }) {
  expanded.value = expanded.value === item.key ? null : item.key
}

function detailText(item: ReturnType<typeof mergeTimeline>[number]): string {
  if (item.mergedKind === 'tool' && item.item) return toolDetail(item.item.Payload)
  if (item.mergedKind) return item.sum
  return item.item ? eventDetail(item.item) : ''
}
</script>

<template>
  <div class="panel">
    <div class="panel-title">
      ⚡ 攻击时间线
      <span v-if="agent" class="badge ok">agent: {{ sessionLabel(agent) }}</span>
      <span class="grow"></span>
      <span class="faint mono">共 {{ combined.length }} 条 · 最早 seq {{ oldestSeq || '-' }}</span>
    </div>

    <div class="row wrap mb8">
      <select
        :value="agent || ''"
        class="session-select"
        @change="onSessionSelect(($event.target as HTMLSelectElement).value)"
      >
        <option value="">全部任务事件</option>
        <option v-for="w in board.workers" :key="w.id" :value="w.id">
          {{ sessionMeta[w.id]?.label || w.worker_type }} · {{ shortId(w.id, 8) }} · {{ w.status }}
        </option>
        <option value="observer">Observer · 旁路审查</option>
      </select>
      <button
        v-for="(label, key) in KIND_LABEL"
        :key="key"
        class="btn sm"
        :class="{ primary: kindFilter === key }"
        @click="kindFilter = key as typeof kindFilter.value"
      >{{ label }}</button>
      <span class="grow"></span>
      <button v-if="agent" class="btn sm ghost" @click="emit('clear-agent')">✕ 显示全部任务事件</button>
      <button class="btn sm ghost" :disabled="!hasMore || loadingOlder" @click="loadOlder">
        {{ loadingOlder ? '加载中…' : '⇡ 加载更早' }}
      </button>
    </div>

    <div v-if="error" class="badge bad mb8">{{ error }}</div>

    <div
      ref="timelineEl"
      class="timeline scroll-y"
      style="max-height: 720px; min-height: 280px; padding-right: 4px"
      @scroll="onScroll"
    >
      <div v-if="visible.length === 0" class="empty">
        <div class="big">⚡</div>
        {{ agent ? '该 agent 暂无事件' : '暂无事件 — 等待任务启动或加载更早历史' }}
      </div>
      <div
        v-for="item in visible"
        :key="item.key"
        class="tl-item"
        :class="{ open: expanded === item.key }"
        @click="toggle(item)"
      >
        <div class="head">
          <span class="badge" :class="kindOf(item.kind).cls">{{ kindOf(item.kind).icon }} {{ kindOf(item.kind).label }}</span>
          <span class="mono faint">{{ fmtClock(item.ts) }}</span>
          <span class="mono faint">#{{ item.seqStart }}</span>
          <span v-if="item.session" class="tag mono" :class="sessionCls(item.session)" :title="item.session">{{ sessionLabel(item.session) }}</span>
          <span class="grow"></span>
          <span class="faint">{{ expanded === item.key ? '▾' : '▸' }}</span>
        </div>
        <div class="sum mt4">{{ item.sum || '(空)' }}</div>
        <div v-if="expanded === item.key" class="tl-detail">
          <div class="faint mono mb8">
            {{ kindOf(item.kind).label }} · seq {{ item.seqStart }}{{ item.seqEnd !== item.seqStart ? `-${item.seqEnd}` : '' }} ·
            session {{ item.session || '-' }} · challenge {{ item.challenge || '-' }}
          </div>
          <pre class="payload">{{ detailText(item) || JSON.stringify(item.item?.Payload || {}, null, 2) }}</pre>
        </div>
      </div>
      <div v-if="!stick && filtered.length > 200" class="faint" style="text-align:center;padding:8px">
        已显示最近 200 条,滚动到底部自动跟随新事件
      </div>
    </div>
  </div>
</template>
