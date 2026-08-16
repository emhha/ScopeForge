package event

import (
	"log/slog"
	"sync"
	"time"

	"scopeforge/internal/store"
)

// Broadcaster 是 SSE 实时推送广播器(docs/05 §1.1):
// 事件落库后同步通知所有订阅者,替代 M0 的 500ms 轮询。
// 慢消费者(chan 满)自动断开 — 客户端以 after=seq 补拉恢复(§2.1 断线续拉语义)。
type Broadcaster struct {
	mu          sync.Mutex
	subscribers map[int64]chan Event
	next        int64
	// buffer 是订阅者 chan 容量(默认 256;SSE 客户端消费慢时先断开再补拉)。
	buffer int
}

// NewBroadcaster 构建广播器。
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subscribers: map[int64]chan Event{}, buffer: 256}
}

// Subscribe 注册订阅,返回事件 chan 与取消函数。
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, b.buffer)
	b.subscribers[id] = ch
	return ch, func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}
}

// SubscriberCount 当前订阅者数(诊断)。
func (b *Broadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// publish 广播一条事件(非阻塞;满则断开该订阅者,客户端 after 补拉)。
func (b *Broadcaster) publish(e Event) {
	b.mu.Lock()
	var drop []int64
	for id, ch := range b.subscribers {
		select {
		case ch <- e:
		default:
			drop = append(drop, id) // 慢消费者:断开,靠补拉恢复
		}
	}
	for _, id := range drop {
		delete(b.subscribers, id)
	}
	b.mu.Unlock()
}

// BroadcastSink 是落库 + 广播的 Sink(M3 装配用,替代裸 SQLiteSink)。
type BroadcastSink struct {
	inner *SQLiteSink
	bc    *Broadcaster
}

// NewBroadcastSink 构建:写库 + 实时通知。
func NewBroadcastSink(db *store.DB, bc *Broadcaster) *BroadcastSink {
	return &BroadcastSink{inner: NewSQLiteSink(db), bc: bc}
}

func (s *BroadcastSink) Emit(e Event) {
	// 补 TS:emitWithSeq 内部对值拷贝补 TS,调用方 e 仍为 0 —
	// 实时 SSE 帧必须带上 TS(否则前端显示 1970-01-01 08:00:00)。
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	seq, err := s.inner.emitWithSeq(e)
	if err != nil {
		// 事件是审计唯一事实源(docs/02 §8.1):落库失败必须告警
		slog.Warn("event persist failed", "kind", string(e.Kind), "error", err)
		return
	}
	if s.bc != nil {
		e.Seq = seq // 回填 DB 分配的 seq,SSE 端单调去重
		s.bc.publish(e)
	}
}
