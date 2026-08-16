package blackboard

import (
	"errors"
	"testing"
	"time"

	"scopeforge/internal/testutil"
)

func openBoard(t *testing.T) *Blackboard {
	t.Helper()
	db := testutil.NewTestDB(t)
	return New(db)
}

func TestFactAppendOnlyAndSupersede(t *testing.T) {
	b := openBoard(t)
	f, err := b.AddFact("c1", PrefixObs, "端口 80 开放", 0.6, StateConfirmed, "w-1", "exec-1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if f.State != StateConfirmed {
		t.Errorf("state = %s", f.State)
	}

	// update:旧行 superseded_by=新行 → 血缘保留(时间旅行)
	if err := b.SupersedeFact("c1", f.ID, "端口 80/443 开放", 0.9, false, "w-2"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	old, err := b.FactByID(f.ID)
	if err != nil {
		t.Fatalf("fact by id: %v", err)
	}
	if old.State != StateSuperseded || old.SupersededBy == 0 {
		t.Errorf("old fact = state %s superseded_by %d, want superseded with successor", old.State, old.SupersededBy)
	}
	newF, err := b.FactByID(old.SupersededBy)
	if err != nil || newF == nil {
		t.Fatalf("successor: %v", err)
	}
	if newF.Text != "端口 80/443 开放" || newF.State != StateConfirmed {
		t.Errorf("successor = %q state %s", newF.Text, newF.State)
	}
	// 血缘链完整
	if newF.Prefix != PrefixObs {
		t.Errorf("prefix lost: %s", newF.Prefix)
	}
	// 历史查询(时间旅行):Facts 只返回 confirmed + superseded
	facts, err := b.Facts("c1", 0)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if len(facts) != 2 {
		t.Errorf("facts = %d, want 2 (old superseded + new)", len(facts))
	}
}

func TestAddFactIdempotent(t *testing.T) {
	b := openBoard(t)
	if _, err := b.AddFact("c1", PrefixVuln, "SQL 注入", 0.8, StateConfirmed, "w-1", "exec-2"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	_, err := b.AddFact("c1", PrefixVuln, "SQL 注入", 0.8, StateConfirmed, "w-1", "exec-3")
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("dup add err = %v, want ErrNoChange", err)
	}
	// 不同 challenge 不冲突
	if _, err := b.AddFact("c2", PrefixVuln, "SQL 注入", 0.8, StateConfirmed, "w-1", "exec-4"); err != nil {
		t.Fatalf("other challenge add: %v", err)
	}
}

func TestSnapshotLimitsAndAsOfSeq(t *testing.T) {
	b := openBoard(t)
	// 15 条 facts(权重递减),10 条 intents
	for i := 0; i < 15; i++ {
		if _, err := b.AddFact("c1", PrefixObs, "fact-"+string(rune('a'+i)), float64(15-i)/15, StateConfirmed, "w-1", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := b.AddIntent("c1", "intent-"+string(rune('a'+i)), float64(10-i)/10, "w-1"); err != nil {
			t.Fatal(err)
		}
	}
	// Phase 2: attempts table removed (deprecated Phase 1 CTF semantics).
	// Snapshot no longer includes attempts.

	snap, err := b.SnapshotForWorker("c1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Facts) > MaxFactsInSnapshot {
		t.Errorf("facts = %d, limit %d", len(snap.Facts), MaxFactsInSnapshot)
	}
	if len(snap.Intents) > MaxIntentsInSnapshot {
		t.Errorf("intents = %d, limit %d", len(snap.Intents), MaxIntentsInSnapshot)
	}
	if len(snap.Facts) == 0 {
		t.Error("expected facts in snapshot")
	}
	// asOfSeq = 当前 seq(facts + intents + 1 起始)
	seq, err := b.CurrentSeq()
	if err != nil {
		t.Fatal(err)
	}
	if snap.AsOfSeq != seq {
		t.Errorf("asOfSeq = %d, current seq = %d", snap.AsOfSeq, seq)
	}
	// 权重排序:快照第一条权重最高
	if snap.Facts[0].Weight < snap.Facts[len(snap.Facts)-1].Weight {
		t.Errorf("facts not sorted by weight desc: first=%v last=%v", snap.Facts[0].Weight, snap.Facts[len(snap.Facts)-1].Weight)
	}
}

func TestSeqMonotonic(t *testing.T) {
	b := openBoard(t)
	s1, _ := b.CurrentSeq()
	if _, err := b.AddFact("c1", PrefixObs, "x", 0.5, StateConfirmed, "w", ""); err != nil {
		t.Fatal(err)
	}
	s2, _ := b.CurrentSeq()
	if s2 != s1+1 {
		t.Errorf("seq %d → %d, want +1", s1, s2)
	}
	if _, err := b.AddIntent("c1", "y", 0.5, "w"); err != nil {
		t.Fatal(err)
	}
	s3, _ := b.CurrentSeq()
	if s3 != s2+1 {
		t.Errorf("seq %d → %d, want +1", s2, s3)
	}
}

func TestIntentLifecycle(t *testing.T) {
	b := openBoard(t)
	it, err := b.AddIntent("c1", "探测 8080", 0.7, "w-1")
	if err != nil {
		t.Fatal(err)
	}
	// 防抖:同文本 open intent 已存在
	if _, err := b.AddIntent("c1", "探测 8080", 0.7, "w-1"); !errors.Is(err, ErrNoChange) {
		t.Errorf("dup intent err = %v, want ErrNoChange", err)
	}
	if err := b.UpdateIntentState(it.ID, StateClaimed, "w-2"); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateIntentState(it.ID, StateDone, ""); err != nil {
		t.Fatal(err)
	}
	intents, err := b.Intents("c1", []string{StateOpen, StateClaimed}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Errorf("open/claimed = %d, want 0 after done", len(intents))
	}
	// done 后可重新添加(新生命周期)
	if _, err := b.AddIntent("c1", "探测 8080", 0.7, "w-1"); err != nil {
		t.Errorf("re-add after done: %v", err)
	}
}

func TestLeaseExclusive(t *testing.T) {
	b := openBoard(t)
	ok, err := b.AcquireLease("reason-c1", "w-1", 100*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	ok, err = b.AcquireLease("reason-c1", "w-2", 10*time.Minute)
	if err != nil || ok {
		t.Fatalf("second acquire should fail: ok=%v err=%v", ok, err)
	}
	// 过期让渡:等待 w-1 租约过期后 w-3 可获取
	time.Sleep(150 * time.Millisecond)
	ok, err = b.AcquireLease("reason-c1", "w-3", 10*time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire after expiry: ok=%v err=%v", ok, err)
	}
	// 释放
	if err := b.ReleaseLease("reason-c1", "w-3"); err != nil {
		t.Fatal(err)
	}
	ok, _ = b.AcquireLease("reason-c1", "w-4", 10*time.Minute)
	if !ok {
		t.Fatal("acquire after release failed")
	}
}

func TestBoardView(t *testing.T) {
	b := openBoard(t)
	view, err := b.SnapshotForScheduler("c1", 0.5)
	if err != nil {
		t.Fatal(err)
	}
	// Phase 2: HasCorrectSubmission removed (was Phase 1 attempt-based).
	if view.FactCount != 0 {
		t.Errorf("fact count = %d", view.FactCount)
	}
	// 低权重 intent 不入视图(权重阈值)
	if _, err := b.AddIntent("c1", "低价值方向", 0.3, "w"); err != nil {
		t.Fatal(err)
	}
	view, err = b.SnapshotForScheduler("c1", 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.OpenIntents) != 0 {
		t.Errorf("open intents = %d, want 0 (weight < 0.5)", len(view.OpenIntents))
	}
	if _, err := b.AddIntent("c1", "高价值方向", 0.9, "w"); err != nil {
		t.Fatal(err)
	}
	view, _ = b.SnapshotForScheduler("c1", 0.5)
	if len(view.OpenIntents) != 1 {
		t.Errorf("open intents = %d, want 1", len(view.OpenIntents))
	}
	// 死路计数
	if _, err := b.AddFact("c1", PrefixDead, "8080 无服务", 0.8, StateConfirmed, "w", ""); err != nil {
		t.Fatal(err)
	}
	view, _ = b.SnapshotForScheduler("c1", 0.5)
	if view.DeadEnds != 1 {
		t.Errorf("dead ends = %d, want 1", view.DeadEnds)
	}
}

func TestWorkerLifecycle(t *testing.T) {
	b := openBoard(t)
	w := &Worker{ID: "w-1", ChallengeID: "c1", WorkerType: "operator", Phase: "explore", SessionID: "s-1",
		Handoff: "handoff", LastProgressAt: time.Now().Unix()}
	if err := b.CreateWorker(w); err != nil {
		t.Fatal(err)
	}
	if err := b.WorkerHeartbeat("w-1", true); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkWorkerFinished("w-1", "done"); err != nil {
		t.Fatal(err)
	}
	ws, err := b.Workers("c1", "running")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Errorf("running workers = %d, want 0", len(ws))
	}
	ws, err = b.Workers("c1", "done")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Status != "done" {
		t.Errorf("done workers = %+v", ws)
	}
}

// ------------------------------------------------------------------ 漏洞账本(phase2 02 §1)

func TestVulnerabilityLedger(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)

	// 写入一条(只增不改)
	v, err := b.AddVulnerability("c1", VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login",
		Severity: "high", Title: "登录口 SQL 注入", EvidenceRef: "exec-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != LedgerSubmitted || v.Severity != "high" || v.Asset != "shop.example.com" {
		t.Fatalf("vuln = %+v", v)
	}

	// 回执状态推进(submitted → accepted),记录不删不改
	if err := b.UpdateVulnerabilityStatus(v.ID, LedgerAccepted, "receipt-1"); err != nil {
		t.Fatal(err)
	}
	got, err := b.VulnerabilityByID(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != LedgerAccepted || got.PlatformRef != "receipt-1" {
		t.Fatalf("after receipt = %+v", got)
	}
	if got.Title != "登录口 SQL 注入" {
		t.Errorf("record mutated: %+v", got)
	}

	// 多条 + 最近一条
	if _, err := b.AddVulnerability("c1", VulnerabilityIn{Asset: "shop.example.com", Endpoint: "/api/order", Title: "越权"}); err != nil {
		t.Fatal(err)
	}
	last, err := b.LastVulnerability("c1")
	if err != nil {
		t.Fatal(err)
	}
	if last.Title != "越权" {
		t.Errorf("last = %+v", last)
	}
	// 空 challenge → nil
	nilV, err := b.LastVulnerability("c-empty")
	if err != nil || nilV != nil {
		t.Fatalf("empty last = %v, %v", nilV, err)
	}

	// 统计
	vs, err := b.Vulnerabilities("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("len = %d", len(vs))
	}
	stats, err := b.LedgerStats("c1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.Accepted != 1 || stats.Submitted != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestVulnerabilityLedgerReceiptDiscipline(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	v, err := b.AddVulnerability("c1", VulnerabilityIn{Asset: "a.example.com", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	// 误报 → 计入 fpr;重复 → 覆盖格仍算已覆盖
	if err := b.UpdateVulnerabilityStatus(v.ID, LedgerFalsePositive, "fp-1"); err != nil {
		t.Fatal(err)
	}
	v2, _ := b.AddVulnerability("c1", VulnerabilityIn{Asset: "a.example.com", Title: "Y"})
	if err := b.UpdateVulnerabilityStatus(v2.ID, LedgerDuplicate, "dup-1"); err != nil {
		t.Fatal(err)
	}
	stats, _ := b.LedgerStats("c1")
	if stats.FalsePositive != 1 || stats.Duplicate != 1 || stats.Accepted != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// ------------------------------------------------------------------ phase2 切片2:幂等键双轨(docs/phase2/03 §1.2)

// TestDedupStructuredKey 验收:"SQLi@shop.example.com/login" 与
// "登录接口注入(SQL)" 二次提交 → 结构化键去重(语义重复被拦)。
func TestDedupStructuredKey(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	_, err := b.AddFact("c1", PrefixVuln, "登录接口存在 SQL 注入", 0.8, StateConfirmed, "w1", "e1",
		FactIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high"})
	if err != nil {
		t.Fatal(err)
	}
	// 同结构化键、不同措辞 → ErrNoChange(语义重复拦截)
	_, err = b.AddFact("c1", PrefixVuln, "SQLi @ /login", 0.7, StateConfirmed, "w2", "e2",
		FactIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high"})
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("structured duplicate = %v, want ErrNoChange", err)
	}
	// 不同 endpoint → 新事实(同 CWE 其他端点不算重复)
	if _, err := b.AddFact("c1", PrefixVuln, "/api/search 注入", 0.8, StateConfirmed, "w1", "e3",
		FactIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/api/search"}); err != nil {
		t.Fatalf("different endpoint should insert: %v", err)
	}
	// 不同 CWE 同端点 → 新事实(相邻漏洞接力前提)
	if _, err := b.AddFact("c1", PrefixVuln, "/login XSS", 0.7, StateConfirmed, "w1", "e4",
		FactIn{CWE: "CWE-79", Asset: "shop.example.com", Endpoint: "/login"}); err != nil {
		t.Fatalf("different cwe should insert: %v", err)
	}
}

// TestDedupStructuredKeyNormalized 归一化兜底:已归一化写入(cwe/asset/endpoint)
// 与已有记录同键(归一化本身由 Dispatcher.normalizeFinding 完成,见 dispatcher_test)。
func TestDedupStructuredKeyNormalized(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	_, err := b.AddFact("c1", PrefixVuln, "原始记录", 0.8, StateConfirmed, "w1", "e1",
		FactIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login"})
	if err != nil {
		t.Fatal(err)
	}
	// 字段漂移经 Dispatcher 归一化后(小写/去 www./去 query/尾部斜杠)→ 同键
	_, err = b.AddFact("c1", PrefixVuln, "漂移写法", 0.6, StateConfirmed, "w2", "e2",
		FactIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login"})
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("normalized duplicate = %v, want ErrNoChange", err)
	}
}

// TestDedupFallbackText 验收:缺 cwe/asset 的 findings 退回逐字幂等,行为不回归。
func TestDedupFallbackText(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	if _, err := b.AddFact("c1", PrefixObs, "端口 80 开放", 0.6, StateConfirmed, "w1", ""); err != nil {
		t.Fatal(err)
	}
	// 逐字重复 → ErrNoChange(阶段一语义保持)
	_, err := b.AddFact("c1", PrefixObs, "端口 80 开放", 0.6, StateConfirmed, "w2", "")
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("text duplicate = %v, want ErrNoChange", err)
	}
	// 结构化字段提供但 cwe 缺失 → 退回逐字(cwe="" 不启用轨 1)
	_, err = b.AddFact("c1", PrefixObs, "端口 80 开放", 0.6, StateConfirmed, "w3", "",
		FactIn{Asset: "shop.example.com", Endpoint: "/"})
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("partial structured duplicate = %v, want ErrNoChange (fallback to text)", err)
	}
	// 同 asset 不同文本 → 新事实(逐字轨不误伤)
	if _, err := b.AddFact("c1", PrefixObs, "端口 443 开放", 0.6, StateConfirmed, "w1", "",
		FactIn{Asset: "shop.example.com"}); err != nil {
		t.Fatalf("different text should insert: %v", err)
	}
}

// TestDedupStructuredAcrossWorkers 不同 worker 上报同漏洞 → 幂等(黑板拓扑健康)。
func TestDedupStructuredAcrossWorkers(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	_, err := b.AddFactsAtomically("c1", -1, []FactIn{
		{Prefix: PrefixVuln, Text: "w1 上报", Weight: 0.8, State: StateConfirmed, CreatedBy: "w1",
			CWE: "CWE-352", Asset: "shop.example.com", Endpoint: "/api/order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.AddFactsAtomically("c1", -1, []FactIn{
		{Prefix: PrefixVuln, Text: "w2 重复上报", Weight: 0.8, State: StateConfirmed, CreatedBy: "w2",
			CWE: "CWE-352", Asset: "shop.example.com", Endpoint: "/api/order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("cross-worker duplicate not deduped: %+v", out)
	}
}

// TestIntentDedupStructured 验收:intents 防抖双轨(target+approach 结构化键)。
func TestIntentDedupStructured(t *testing.T) {
	db := testutil.NewTestDB(t)
	b := New(db)
	_, err := b.AddIntent("c1", "对 /api/order 测试越权", 0.6, "w1", IntentIn{Target: "/api/order", Approach: "IDOR"})
	if err != nil {
		t.Fatal(err)
	}
	// 同 target+approach 不同措辞 → ErrNoChange
	_, err = b.AddIntent("c1", "再测一次越权", 0.6, "w2", IntentIn{Target: "/api/order", Approach: "IDOR"})
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("structured intent duplicate = %v, want ErrNoChange", err)
	}
	// 同 target 不同 approach → 新 intent(多样性)
	if _, err := b.AddIntent("c1", "对 /api/order 测注入", 0.6, "w1", IntentIn{Target: "/api/order", Approach: "SQLi"}); err != nil {
		t.Fatalf("different approach should insert: %v", err)
	}
	// 无结构化 → 逐字防抖(阶段一回归)
	if _, err := b.AddIntent("c1", "自由方向", 0.5, "w1"); err != nil {
		t.Fatal(err)
	}
	_, err = b.AddIntent("c1", "自由方向", 0.5, "w2")
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("text intent duplicate = %v, want ErrNoChange", err)
	}
	// 读取含 target/approach
	intents, _ := b.Intents("c1", []string{StateOpen}, 0)
	found := false
	for _, it := range intents {
		if it.Target == "/api/order" && it.Approach == "IDOR" {
			found = true
		}
	}
	if !found {
		t.Errorf("structured intent not persisted: %+v", intents)
	}
}
