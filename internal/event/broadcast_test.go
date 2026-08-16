package event

import (
	"testing"
	"time"

	"scopeforge/internal/testutil"
)

func TestBroadcasterFanOut(t *testing.T) {
	bc := NewBroadcaster()
	ch1, cancel1 := bc.Subscribe()
	ch2, _ := bc.Subscribe()
	defer cancel1()

	bc.publish(Event{Seq: 1, Kind: KindTurnStart})
	select {
	case e := <-ch1:
		if e.Seq != 1 {
			t.Errorf("ch1 seq=%d", e.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 no event")
	}
	select {
	case e := <-ch2:
		if e.Seq != 1 {
			t.Errorf("ch2 seq=%d", e.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 no event")
	}
}

func TestBroadcastSinkPersistsAndPublishes(t *testing.T) {
	db := testutil.NewTestDB(t)
	bc := NewBroadcaster()
	sink := NewBroadcastSink(db, bc)
	ch, cancel := bc.Subscribe()
	defer cancel()

	sink.Emit(Event{Kind: KindToolCallStart, SessionID: "s1", Payload: map[string]any{"x": 1}})
	// 落库
	evs, err := Query(db, "", 0, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("persist: %v %v", evs, err)
	}
	// 广播
	select {
	case e := <-ch:
		if e.Kind != KindToolCallStart {
			t.Errorf("kind=%s", e.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no broadcast")
	}
}

func TestSlowConsumerDisconnected(t *testing.T) {
	bc := NewBroadcaster()
	bc.buffer = 2
	ch, cancel := bc.Subscribe()
	defer cancel()
	// 塞满缓冲(第 3 条时慢消费者被断开)
	for i := 0; i < 3; i++ {
		bc.publish(Event{Seq: int64(i)})
	}
	// 断开后新事件不再送达(chan 里只剩断开前的旧事件)
	bc.publish(Event{Seq: 99})
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-ch:
			if e.Seq == 99 {
				t.Fatal("slow consumer should be disconnected, got seq=99")
			}
		case <-deadline:
			return // 旧事件读完且无 seq=99 → 正确
		}
	}
}
