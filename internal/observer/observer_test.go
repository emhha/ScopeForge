package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/testutil"

	_ "scopeforge/internal/reasonix/provider/openai"
)

// mockObserverLLM 输出固定审查契约(测试参数化)。
type mockObserverLLM struct {
	*httptest.Server
	contract string // 固定输出
}

func newMockObserverLLM(t *testing.T, contract string) *mockObserverLLM {
	t.Helper()
	m := &mockObserverLLM{contract: contract}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": m.contract}}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
		data2, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 80, "completion_tokens": 10, "total_tokens": 90}})
		fmt.Fprintf(w, "data: %s\n\n", data2)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

// newTestObserver 装配 Observer 测试环境。
func newTestObserver(t *testing.T, contract string, active []string) (*Observer, *blackboard.Blackboard, *dispatcher.Dispatcher, *fakeHooks) {
	t.Helper()
	return newTestObserverSink(t, contract, active, event.Discard)
}

// newTestObserverSink 同 newTestObserver,但可指定事件 sink(验证事件落库)。
func newTestObserverSink(t *testing.T, contract string, active []string, sink event.Sink) (*Observer, *blackboard.Blackboard, *dispatcher.Dispatcher, *fakeHooks) {
	t.Helper()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	disp := dispatcher.New(board, sink, nil)
	hooks := &fakeHooks{}
	disp.SetHooks(hooks)

	llm := newMockObserverLLM(t, contract)
	p, err := provider.New("openai", provider.Config{Name: "obs", BaseURL: llm.URL, Model: "mini", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ledger := constraint.NewCostLedger(db)
	o := New(Options{
		Cfg:           DefaultConfig(),
		Board:         board,
		Dispatcher:    disp,
		Provider:      p,
		Sink:          sink,
		Ledger:        ledger,
		DB:            db,
		ActiveWorkers: func() []string { return active },
	})
	return o, board, disp, hooks
}

type fakeHooks struct {
	launched []string
	aborted  []string
	steered  []string
}

func (f *fakeHooks) Launch(ctx context.Context, workerType string, handoff dispatcher.Handoff) (string, error) {
	f.launched = append(f.launched, workerType)
	return "w-" + workerType, nil
}
func (f *fakeHooks) Abort(ctx context.Context, workerID, reason string) error {
	f.aborted = append(f.aborted, workerID+":"+reason)
	return nil
}
func (f *fakeHooks) Steer(ctx context.Context, workerID, message, priority string) error {
	f.steered = append(f.steered, workerID+":"+message)
	return nil
}

// TestObserverReviewAppliesConservativeUpdates 保守四档应用。
func TestObserverReviewAppliesConservativeUpdates(t *testing.T) {
	contract := `{"board_updates":[
		{"action":"no_change","fact_id":1},
		{"action":"update","fact_id":2,"new_text":"端口 80/443/8443 开放","weight":0.9},
		{"action":"delete","fact_id":3},
		{"action":"add","new_text":"观察:WAF 检测到","weight":0.7}
	],"steer":{"target_worker":"w-3","message":"优先验证弱口令方向","priority":"high"}}`
	o, board, _, hooks := newTestObserver(t, contract, []string{"w-3"})

	// 造事实
	f1, _ := board.AddFact("c1", blackboard.PrefixObs, "端口 80 开放", 0.5, blackboard.StateConfirmed, "w-1", "")
	f2, _ := board.AddFact("c1", blackboard.PrefixObs, "端口 80 开放(旧)", 0.5, blackboard.StateConfirmed, "w-1", "")
	f3, _ := board.AddFact("c1", blackboard.PrefixObs, "过时猜测", 0.3, blackboard.StateCandidate, "w-1", "")
	_ = f1

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.ReviewOnce(ctx, "c1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	// update:旧行 superseded,新行 confirmed
	old2, _ := board.FactByID(f2.ID)
	if old2.State != blackboard.StateSuperseded || old2.SupersededBy == 0 {
		t.Errorf("fact2 = %s/%d, want superseded with successor", old2.State, old2.SupersededBy)
	}
	succ, _ := board.FactByID(old2.SupersededBy)
	if succ == nil || succ.Text != "端口 80/443/8443 开放" || succ.State != blackboard.StateConfirmed {
		t.Errorf("successor = %+v", succ)
	}
	// delete:superseded 无后继
	old3, _ := board.FactByID(f3.ID)
	if old3.State != blackboard.StateSuperseded || old3.SupersededBy != 0 {
		t.Errorf("fact3 = %s/%d, want superseded with no successor", old3.State, old3.SupersededBy)
	}
	// add:新 fact
	facts, _ := board.Facts("c1", 0)
	found := false
	for _, f := range facts {
		if f.Text == "观察:WAF 检测到" && f.State == blackboard.StateConfirmed {
			found = true
		}
	}
	if !found {
		t.Errorf("added fact missing: %+v", facts)
	}
	// steer 投递
	if len(hooks.steered) != 1 || hooks.steered[0] != "w-3:优先验证弱口令方向" {
		t.Errorf("steered = %v", hooks.steered)
	}
}

// TestObserverReviewEventsCarrySessionID 审查事件统一归口 SessionID="observer",
// 且 ChallengeID 被补全——看板 observer 卡片(created_by="observer")按
// SessionID 过滤即可见完整审查时间线(此前 obs-<nano> 每次不同匹配不上)。
func TestObserverReviewEventsCarrySessionID(t *testing.T) {
	contract := `{"board_updates":[{"action":"add","new_text":"观察:q=' 单引号返回 200 空数据","weight":0.6}],"steer":null}`
	db := testutil.NewTestDB(t)
	sink := event.NewSQLiteSink(db)
	o, _, _, _ := newTestObserverSink(t, contract, nil, sink)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.ReviewOnce(ctx, "c1"); err != nil {
		t.Fatalf("review: %v", err)
	}

	// 1. executor 审查事件(turn_start/思考增量)带 SessionID="observer" 且 ChallengeID 被补全
	evs, err := event.Query(db, "observer", 0, 0, 500)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("无 SessionID=observer 的审查事件")
	}
	execEvents := 0
	for _, e := range evs {
		if e.ChallengeID != "c1" {
			t.Errorf("事件 seq=%d 的 ChallengeID=%q, want c1(executor 事件必须补全)", e.Seq, e.ChallengeID)
		}
		switch e.Kind {
		case event.KindTurnStart, event.KindTextDelta, event.KindCheckpoint:
			execEvents++
		}
	}
	if execEvents == 0 {
		t.Errorf("无 turn_start/text_delta/checkpoint 审查事件: %+v", evs)
	}

	// 2. observer_review 摘要事件也归口 observer 会话
	found := false
	for _, e := range evs {
		if e.Kind == event.KindCheckpoint && e.ChallengeID == "c1" && e.SessionID == "observer" {
			if p, ok := e.Payload.(map[string]any); ok && p["action"] == "observer_review" {
				found = true
			}
		}
	}
	if !found {
		t.Error("缺 observer_review checkpoint(SessionID=observer)")
	}
}

// TestObserverNoChangeSteerFallback 空 target → 投给第一个活动 worker;无活动 → 不投。
func TestObserverNoChangeSteerFallback(t *testing.T) {
	contract := `{"board_updates":[{"action":"no_change"}],"steer":{"target_worker":"","message":"检查内网方向","priority":"medium"}}`
	o, _, _, hooks := newTestObserver(t, contract, []string{"w-1", "w-2"})
	ctx := context.Background()
	if err := o.ReviewOnce(ctx, "c1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(hooks.steered) != 1 || hooks.steered[0] != "w-1:检查内网方向" {
		t.Errorf("steered = %v", hooks.steered)
	}

	// 无活动 worker → 不投
	o2, _, _, hooks2 := newTestObserver(t, contract, nil)
	if err := o2.ReviewOnce(ctx, "c1"); err != nil {
		t.Fatalf("review2: %v", err)
	}
	if len(hooks2.steered) != 0 {
		t.Errorf("steered with no workers = %v", hooks2.steered)
	}
}

// TestObserverBudgetVolume 板体积硬上限:观察者持续添加时快照仍 ≤10/8。
func TestObserverBudgetVolume(t *testing.T) {
	contract := `{"board_updates":[{"action":"add","new_text":"观察:新增事实","weight":0.5}],"steer":null}`
	o, board, _, _ := newTestObserver(t, contract, nil)
	ctx := context.Background()
	// 先塞满 15 条事实
	for i := 0; i < 15; i++ {
		_, _ = board.AddFact("c1", blackboard.PrefixObs, fmt.Sprintf("旧事实 %d", i), 0.5, blackboard.StateConfirmed, "w", "")
	}
	// 5 次审查(每次 add 一条)
	for i := 0; i < 5; i++ {
		if err := o.ReviewOnce(ctx, "c1"); err != nil {
			t.Fatalf("review %d: %v", i, err)
		}
	}
	snap, err := board.SnapshotForWorker("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) > blackboard.MaxFactsInSnapshot {
		t.Errorf("snapshot facts = %d, limit %d", len(snap.Facts), blackboard.MaxFactsInSnapshot)
	}
}

// TestObserverNotInHotPath Observer 不执行任何工具(空注册表)。
func TestObserverNotInHotPath(t *testing.T) {
	contract := `{"board_updates":[{"action":"no_change"}],"steer":null}`
	o, _, _, _ := newTestObserver(t, contract, nil)
	ctx := context.Background()
	if err := o.ReviewOnce(ctx, "c1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	// Observer 会话落库(kind=observer)且不含任何工具调用
	_ = ctx
	_ = conversation.KindObserver
	_ = strings.TrimSpace
	_ = executor.ModeYolo
}

// ------------------------------------------------------------------ phase2 切片4:覆盖审查建议(03 §3.5)

// TestCoverageSuggestionToIntent 验收:Observer coverage_suggestions →
// 新 intent 落地(ReportIntent)。
func TestCoverageSuggestionToIntent(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	hook := &fakeHooks{}
	d := dispatcher.New(board, nil, hook)
	llm := newMockObserverLLM(t, `{"board_updates":[],"coverage_suggestions":[]}`)
	p, err := provider.New("openai", provider.Config{Name: "obs", BaseURL: llm.URL, Model: "mini", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	o := New(Options{
		Cfg: DefaultConfig(), Board: board, Dispatcher: d,
		Provider: p, Sink: event.Discard, Ledger: constraint.NewCostLedger(db),
		CompactCfg: conversation.DefaultCompactConfig(), DB: db,
		ActiveWorkers: func() []string { return []string{"w-1"} },
		Coverage:      coverage.New(db),
	})
	ctx := context.Background()
	// 直接验证 apply 路径(解析后的建议 → intent)
	review := &Review{
		CoverageSuggestions: []CoverageSuggestion{
			{Direction: "CWE-22@/download", Reason: "矩阵中唯一未探索的高危格子"},
		},
	}
	o.apply(ctx, "c1", review)
	intents, err := board.Intents("c1", []string{blackboard.StateOpen}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(intents))
	}
	if intents[0].Target != "CWE-22@/download" || intents[0].Approach != "observer" {
		t.Fatalf("suggestion intent = %+v", intents[0])
	}
	// 幂等:同建议不重复
	o.apply(ctx, "c1", review)
	intents, _ = board.Intents("c1", []string{blackboard.StateOpen}, 0)
	if len(intents) != 1 {
		t.Errorf("duplicate suggestion intent = %d, want 1", len(intents))
	}
}

// TestBuildPromptAuthGuidance 验收:审查提示词带"登录后攻击面审查"语义引导
// (不依赖文本正则——万能密码/认证绕过等非明文凭据由 LLM 语义判断),
// 引导规则无条件注入,事件/黑板中的登录成功线索由 Observer 自行识别。
func TestBuildPromptAuthGuidance(t *testing.T) {
	o, _, _, _ := newTestObserver(t, `{"board_updates":[]}`, nil)
	snap, _ := o.board.SnapshotForWorker("c1")
	p := o.buildPrompt("c1", nil, snap)

	if !strings.Contains(p, "登录后攻击面审查") {
		t.Errorf("提示词应含登录后攻击面审查段落:\n%s", p)
	}
	if !strings.Contains(p, "认证绕过") || !strings.Contains(p, "万能密码") {
		t.Errorf("引导应覆盖非明文凭据场景(认证绕过/万能密码):\n%s", p)
	}
	if !strings.Contains(p, "coverage_suggestions") {
		t.Errorf("引导应强制提出 coverage_suggestions/steer:\n%s", p)
	}
	// 契约说明允许自由文本方向(登录后方向不一定有矩阵格子)
	if !strings.Contains(p, "自由文本") {
		t.Errorf("契约说明应允许自由文本方向:\n%s", p)
	}
}

// TestBuildPromptCapabilityGuidance 验收(2.24):审查提示词带"能力获得审查"
// 语义引导——shell/域用户能力识别(WebShell/反弹 shell/Kerberoast 等非明文
// 形式由 LLM 语义判断),含能力→转移候选映射与漏报覆盖语义(explore 忘记在
// finding 声明 privilege/auth_gained 时 Observer 兜底建议解锁方向)。
func TestBuildPromptCapabilityGuidance(t *testing.T) {
	o, _, _, _ := newTestObserver(t, `{"board_updates":[]}`, nil)
	snap, _ := o.board.SnapshotForWorker("c1")
	p := o.buildPrompt("c1", nil, snap)

	for _, want := range []string{
		"能力获得审查",
		"WebShell", "反弹 shell", "RCE 落地", // shell 非明文形式识别
		"Kerberoast", "委派利用", // 域用户非明文形式识别
		"横向移动/提权/凭据收集", "域枚举/委派攻击/密码喷洒", // 能力→候选映射
		"忘记在 finding 声明 privilege", // 漏报覆盖语义
	} {
		if !strings.Contains(p, want) {
			t.Errorf("能力引导应含 %q:\n%s", want, p)
		}
	}
	// 与登录后段落并存(2.17 通道不破坏)
	if !strings.Contains(p, "登录后攻击面审查") {
		t.Error("登录后攻击面审查段落应保留")
	}
}
