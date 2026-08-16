package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scopeforge/internal/event"
	"scopeforge/internal/testutil"
)

func TestHealth(t *testing.T) {
	db := testutil.NewTestDB(t)
	srv := httptest.NewServer(NewServer(Deps{DB: db}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var h healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" {
		t.Errorf("health = %+v", h)
	}
}

func TestEventsIncrementalPull(t *testing.T) {
	db := testutil.NewTestDB(t)
	// 写入事件
	sink := event.NewSQLiteSink(db)
	sink.Emit(event.Event{Kind: event.KindTurnStart, SessionID: "s1", Payload: map[string]any{"turn": 0}})
	sink.Emit(event.Event{Kind: event.KindTextDelta, SessionID: "s1", Payload: map[string]any{"text": "hi"}})
	sink.Emit(event.Event{Kind: event.KindTurnStart, SessionID: "s2", Payload: map[string]any{"turn": 0}})

	srv := httptest.NewServer(NewServer(Deps{DB: db}).Handler())
	defer srv.Close()

	// 全量
	resp, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Events []event.Event `json:"events"`
		Latest int64         `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Events) != 3 || out.Latest != 3 {
		t.Errorf("events = %d latest = %d", len(out.Events), out.Latest)
	}

	// 增量:after=2 → 只有 seq 3
	resp, err = http.Get(srv.URL + "/api/v1/events?after=2")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Events) != 1 || out.Events[0].Seq != 3 {
		t.Errorf("incremental = %+v", out.Events)
	}

	// 按会话过滤
	resp, err = http.Get(srv.URL + "/api/v1/events?session=s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Events) != 2 {
		t.Errorf("session filter = %d", len(out.Events))
	}
}

func TestEventsMergeCompressesDeltas(t *testing.T) {
	db := testutil.NewTestDB(t)
	sink := event.NewSQLiteSink(db)
	for _, text := range []string{"_", "re", "_", "x"} {
		sink.Emit(event.Event{Kind: event.KindReasoningDelta, SessionID: "w", Payload: map[string]any{"text": text}})
	}
	sink.Emit(event.Event{Kind: event.KindTurnStart, SessionID: "w", Payload: map[string]any{"turn": 1}})

	srv := httptest.NewServer(NewServer(Deps{DB: db}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/events?before=9007199254740991&merge=1&session=w&limit=100")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Events   []event.Event `json:"events"`
		RawCount int           `json:"raw_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RawCount != 5 {
		t.Fatalf("raw_count = %d, want 5", out.RawCount)
	}
	if len(out.Events) != 2 {
		t.Fatalf("merged events = %d, want 2(delta 块 + turn_start)", len(out.Events))
	}
	delta := out.Events[1]
	if delta.Payload.(map[string]any)["text"] != "_re_x" || delta.SeqEnd != 4 {
		t.Fatalf("merged block = %+v", delta)
	}
}

func TestSSEStream(t *testing.T) {
	db := testutil.NewTestDB(t)
	srv := httptest.NewServer(NewServer(Deps{DB: db}).Handler())
	defer srv.Close()

	// SSE 长连接:后台写入事件,客户端应收到
	done := make(chan string, 1)
	go func() {
		// 用普通 http.Get 读流(会阻塞直到事件)
		resp, err := http.Get(srv.URL + "/api/v1/events/stream?after=0")
		if err != nil {
			done <- "err:" + err.Error()
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()

	// 等待客户端连上后写入事件
	time.Sleep(200 * time.Millisecond)
	sink := event.NewSQLiteSink(db)
	sink.Emit(event.Event{Kind: event.KindTextDelta, SessionID: "sse", Payload: map[string]any{"text": "streamed"}})

	select {
	case got := <-done:
		if !strings.Contains(got, "streamed") {
			t.Errorf("sse payload = %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sse timeout")
	}
}
