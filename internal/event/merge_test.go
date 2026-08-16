package event

import (
	"strings"
	"testing"

	"scopeforge/internal/store"
)

func TestMergeDeltasCompressesHistory(t *testing.T) {
	db, err := store.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	sink := NewSQLiteSink(db)
	for i, text := range []string{"_", "re", "_", "a", "b", "c"} {
		sink.Emit(Event{Kind: KindReasoningDelta, SessionID: "w", ChallengeID: "c1", Payload: map[string]any{"text": text}, TS: int64(i + 1)})
	}
	sink.Emit(Event{Kind: KindTurnStart, SessionID: "w", ChallengeID: "c1", Payload: map[string]any{"turn": 2}, TS: 7})

	events, err := Query(db, "w", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("raw events = %d, want 7", len(events))
	}
	merged := MergeDeltas(events)
	if len(merged) != 2 {
		t.Fatalf("merged events = %d, want 2(6 个 delta 合 1 块 + turn_start)", len(merged))
	}
	text := deltaText(merged[0].Payload)
	if text != "_re_abc" || merged[0].Seq != events[0].Seq || merged[0].SeqEnd != events[5].Seq {
		t.Fatalf("merged block = %q seq=%d end=%d", text, merged[0].Seq, merged[0].SeqEnd)
	}

	// before 查询是降序;MergeDeltas 保持降序且文本顺序正确。
	desc, err := Query(db, "w", 0, 1<<30, 100)
	if err != nil {
		t.Fatal(err)
	}
	if desc[0].Seq <= desc[1].Seq {
		t.Fatal("expected desc query order")
	}
	mergedDesc := MergeDeltas(desc)
	if mergedDesc[0].Seq <= mergedDesc[len(mergedDesc)-1].Seq {
		t.Fatal("MergeDeltas must preserve desc order")
	}
	if !strings.Contains(deltaText(mergedDesc[len(mergedDesc)-1].Payload), "_re_abc") {
		t.Fatalf("desc merged text = %q", deltaText(mergedDesc[len(mergedDesc)-1].Payload))
	}
}
