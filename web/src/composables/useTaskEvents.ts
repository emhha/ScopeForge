// 任务事件快照:SSE 事件按 revision 节流同步。
// 攻击时间线在运行期会以每秒几十条的频率收到 reasoning/text 增量;
// 若 computed 直接依赖 bucket,每个 SSE 事件都会触发全量排序 + 时间线合并,
// 页面会卡到任务结束。这里把同步频率收敛到 500ms 一档,展示层只依赖快照。
import { onUnmounted, ref, watch } from 'vue'
import type { EventItem } from '../api/types'
import { useBus } from '../stores/bus'

export function useTaskEvents(
  challenge: () => string,
  agent: () => string | null = () => null,
  throttleMs = 500,
) {
  const bus = useBus()
  const events = ref<EventItem[]>([])
  let timer: ReturnType<typeof setTimeout> | undefined

  function sync() {
    const all = bus.eventsFor(challenge())
    const id = agent()
    events.value = id ? all.filter((e) => e.SessionID === id) : all
  }

  function schedule() {
    window.clearTimeout(timer)
    timer = setTimeout(sync, throttleMs)
  }

  watch([challenge, agent], () => {
    sync()
    schedule()
  }, { immediate: true })

  watch(
    () => bus.revision,
    () => schedule(),
  )

  onUnmounted(() => window.clearTimeout(timer))

  return { events, sync }
}
