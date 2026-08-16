package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/coverage"
	"scopeforge/internal/event"
	"scopeforge/internal/testutil"
)

// fakeHooks 记录 Launch/Abort/Steer 调用。
type fakeHooks struct {
	launched []string
	aborted  []string
	steered  []string
}

func (f *fakeHooks) Launch(ctx context.Context, workerType string, handoff Handoff) (string, error) {
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

func newTestDispatcher(t *testing.T) (*Dispatcher, *blackboard.Blackboard, *fakeHooks) {
	t.Helper()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	hooks := &fakeHooks{}
	d := New(board, nil, hooks)
	return d, board, hooks
}

func TestReportFindingsAndIdempotency(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, err := board.SnapshotForWorker("easy-1")
	if err != nil {
		t.Fatal(err)
	}
	findings := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "登录口存在 SQL 注入", Weight: 0.8, EvidenceRef: "exec-1"},
		{Prefix: blackboard.PrefixObs, Text: "端口 80 开放", Weight: 0.6, EvidenceRef: "exec-2"},
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, findings); err != nil {
		t.Fatalf("report: %v", err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	// 幂等:worker 合并重报(重读快照携带新 asOfSeq)→ 不产生重复 fact
	snap2, err := board.SnapshotForWorker("easy-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap2.AsOfSeq, findings); err != nil {
		t.Fatalf("re-report: %v", err)
	}
	facts, _ = board.Facts("easy-1", 0)
	if len(facts) != 2 {
		t.Errorf("after dup report facts = %d, want 2 (idempotent)", len(facts))
	}
}

func TestReportFindingsSeqConflict(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, err := board.SnapshotForWorker("easy-1")
	if err != nil {
		t.Fatal(err)
	}
	// 他人写入推进 seq
	if _, err := board.AddFact("easy-1", blackboard.PrefixObs, "他人写入", 0.5, blackboard.StateConfirmed, "w-other", ""); err != nil {
		t.Fatal(err)
	}
	err = d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "发现 X", Weight: 0.7},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// 合并重报(携带新 seq)成功
	snap2, _ := board.SnapshotForWorker("easy-1")
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap2.AsOfSeq, []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "发现 X", Weight: 0.7},
	}); err != nil {
		t.Fatalf("re-report after merge: %v", err)
	}
}

func TestUpdateFactConservativeModes(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	// add
	if err := d.UpdateFact(context.Background(), "easy-1", Mutation{Action: "add", NewText: "观察:服务在 8080", Weight: 0.5}); err != nil {
		t.Fatal(err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Fatalf("facts = %d", len(facts))
	}
	// no_change 不写
	if err := d.UpdateFact(context.Background(), "easy-1", Mutation{Action: "no_change"}); err != nil {
		t.Fatal(err)
	}
	facts, _ = board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Errorf("no_change wrote: %d", len(facts))
	}
	// update → 旧行 superseded
	if err := d.UpdateFact(context.Background(), "easy-1", Mutation{Action: "update", FactID: facts[0].ID, NewText: "观察:服务在 8080/8443", Weight: 0.9}); err != nil {
		t.Fatal(err)
	}
	old, _ := board.FactByID(facts[0].ID)
	if old.State != blackboard.StateSuperseded || old.SupersededBy == 0 {
		t.Errorf("old fact = %s/%d", old.State, old.SupersededBy)
	}
	// delete → superseded 无后继
	cur, _ := board.Facts("easy-1", 0)
	for _, f := range cur {
		if f.State == blackboard.StateConfirmed {
			if err := d.UpdateFact(context.Background(), "easy-1", Mutation{Action: "delete", FactID: f.ID}); err != nil {
				t.Fatal(err)
			}
			del, _ := board.FactByID(f.ID)
			if del.State != blackboard.StateSuperseded || del.SupersededBy != 0 {
				t.Errorf("deleted fact = %s/%d", del.State, del.SupersededBy)
			}
			break
		}
	}
	// 非法 action
	if err := d.UpdateFact(context.Background(), "easy-1", Mutation{Action: "explode"}); err == nil {
		t.Error("unknown action should error")
	}
}

func TestSchedulerHooksAndSteer(t *testing.T) {
	d, _, hooks := newTestDispatcher(t)
	// Launch
	wid, err := d.Launch(context.Background(), "operator", Handoff{ChallengeID: "easy-1"})
	if err != nil {
		t.Fatal(err)
	}
	if wid != "w-operator" || len(hooks.launched) != 1 {
		t.Errorf("launch = %q hooks=%v", wid, hooks.launched)
	}
	// Abort
	if err := d.Abort(context.Background(), "easy-1", "w-operator", "stale"); err != nil {
		t.Fatal(err)
	}
	// Steer(空消息忽略)
	if err := d.Steer(context.Background(), "easy-1", "w-operator", Steer{Message: ""}); err != nil {
		t.Fatal(err)
	}
	if err := d.Steer(context.Background(), "easy-1", "w-operator", Steer{TargetWorker: "w-3", Message: "优先验证弱口令", Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.steered) != 1 || hooks.steered[0] != "w-operator:优先验证弱口令" {
		t.Errorf("steered = %v", hooks.steered)
	}
}

func TestParseContract(t *testing.T) {
	// 纯 JSON
	c, err := ParseContract(`{"accepted":true,"findings":[{"prefix":"vuln:","text":"SQLi","weight":0.8,"evidence_ref":"exec-1"}],"new_intents":[{"text":"注入利用","weight":0.9}],"dead_ends":["x"],"stop_reason":"exhausted"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Findings) != 1 || c.Findings[0].Prefix != "vuln:" || c.StopReason != "exhausted" || len(c.NewIntents) != 1 {
		t.Errorf("contract = %+v", c)
	}
	// 围栏包裹
	c2, err := ParseContract("思考过程...\n```json\n{\"accepted\":true,\"stop_reason\":\"conclude\"}\n```\n结束")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Accepted || c2.StopReason != "conclude" {
		t.Errorf("fenced contract = %+v", c2)
	}
	// 文本夹杂(大括号配对兜底)
	c3, err := ParseContract("我完成了: {\"accepted\": true, \"stop_reason\": \"intent_done\", \"dead_ends\": [\"a\",\"b\"]} 再见")
	if err != nil {
		t.Fatal(err)
	}
	if c3.StopReason != "intent_done" || len(c3.DeadEnds) != 2 {
		t.Errorf("inline contract = %+v", c3)
	}
	// auth_gained 结构化字段(登录态/凭据声明,含万能密码/绕过场景)
	c4, err := ParseContract(`{"accepted":true,"findings":[{"prefix":"obs:","text":"万能密码 ' OR '1'='1 登录成功","weight":0.9,"evidence_ref":"exec-1","auth_gained":true}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c4.Findings) != 1 || !c4.Findings[0].AuthGained {
		t.Errorf("auth_gained should parse: %+v", c4.Findings)
	}
	// 缺省 = false(旧契约兼容)
	c5, err := ParseContract(`{"accepted":true,"findings":[{"prefix":"obs:","text":"x"}],"stop_reason":"intent_done"}`)
	if err != nil {
		t.Fatal(err)
	}
	if c5.Findings[0].AuthGained {
		t.Error("缺省 auth_gained 应为 false")
	}
	// privilege 通用能力声明(2.23:shell/域用户等权限形态,逗号分隔可多个)
	c6, err := ParseContract(`{"accepted":true,"findings":[{"prefix":"obs:","text":"getshell","weight":0.9,"evidence_ref":"exec-2","asset":"web-host","privilege":"shell,authenticated"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c6.Findings) != 1 || c6.Findings[0].Privilege != "shell,authenticated" {
		t.Errorf("privilege should parse: %+v", c6.Findings)
	}
	// 缺省 = 空(旧契约兼容)
	if c5.Findings[0].Privilege != "" {
		t.Error("缺省 privilege 应为空")
	}
	// 非 JSON
	if _, err := ParseContract("完全不是 JSON 的输出"); err == nil {
		t.Error("non-JSON should error")
	}
}

func TestReportVulnerabilityAndReceipt(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	v, err := d.ReportVulnerability(context.Background(), "w-1", "easy-1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != blackboard.LedgerSubmitted {
		t.Fatalf("status = %s", v.Status)
	}
	// 回执推进
	if err := d.UpdateVulnerabilityReceipt(context.Background(), "w-1", "easy-1", v.ID, blackboard.LedgerAccepted, "r1"); err != nil {
		t.Fatal(err)
	}
	got, _ := board.VulnerabilityByID(v.ID)
	if got.Status != blackboard.LedgerAccepted || got.PlatformRef != "r1" {
		t.Fatalf("after receipt = %+v", got)
	}
}

func TestParseContractExhaustion(t *testing.T) {
	// S6 穷尽声明契约(docs/phase2/02 §2.4)
	text := `{"accepted":true,"exhausted":true,
	  "coverage_evidence":[{"direction":"CWE-89@/login","tried":["error_based","union"],"excluded":"参数全参数化"}],
	  "remaining_risks":["CWE-22@/download 未测(需登录态)"],"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`
	c, err := ParseContract(text)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Exhausted || len(c.CoverageEvidence) != 1 || len(c.RemainingRisks) != 1 {
		t.Fatalf("contract = %+v", c)
	}
	if c.CoverageEvidence[0].Direction != "CWE-89@/login" || len(c.CoverageEvidence[0].Tried) != 2 {
		t.Errorf("evidence = %+v", c.CoverageEvidence[0])
	}
	// 无证据的穷尽声明(不采信场景)
	c2, err := ParseContract(`{"exhausted":true,"coverage_evidence":[],"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Exhausted || len(c2.CoverageEvidence) != 0 {
		t.Fatalf("c2 = %+v", c2)
	}
	// 实测回归:模型把 coverage_evidence 输出为单对象(非数组)时,
	// 契约必须仍能解析(否则 conclude worker unparseable 反复重试)。
	c3, err := ParseContract(`{"accepted":true,"exhausted":true,
	  "coverage_evidence":{"direction":"CWE-89@/rest/products/search","tried":["union","boolean_blind"],"excluded":"已确认注入点"},
	  "findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`)
	if err != nil {
		t.Fatalf("coverage_evidence 单对象应可解析: %v", err)
	}
	if !c3.Exhausted || len(c3.CoverageEvidence) != 1 {
		t.Fatalf("c3 = %+v", c3)
	}
	if c3.CoverageEvidence[0].Direction != "CWE-89@/rest/products/search" {
		t.Errorf("c3 evidence = %+v", c3.CoverageEvidence[0])
	}
}

// TestReportVulnerabilityDedupe 验收:同 challenge 归一化键重复提交 →
// 返回 duplicate 记录且不新增账本行(实测:同一漏洞被重复提交 5 次)。
// 误报/拒绝记录不挡重试;跨 challenge 不误伤。
func TestReportVulnerabilityDedupe(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	ctx := context.Background()
	first, err := d.ReportVulnerability(ctx, "w-1", "easy-1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "http://192.168.81.167:3000", Endpoint: "/rest/products/search?q=",
		Severity: "high", Title: "SQLi in q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != blackboard.LedgerSubmitted {
		t.Fatalf("first status = %s", first.Status)
	}
	// 同漏洞、不同 endpoint 写法(方法前缀/完整 URL)→ 归一化后仍判重复
	for _, ep := range []string{"GET /rest/products/search?q=", "http://192.168.81.167:3000/rest/products/search?q="} {
		dup, err := d.ReportVulnerability(ctx, "w-2", "easy-1", blackboard.VulnerabilityIn{
			CWE: "CWE-89", Asset: "192.168.81.167:3000", Endpoint: ep,
			Severity: "high", Title: "SQLi again",
		})
		if err != nil {
			t.Fatal(err)
		}
		if dup.Status != blackboard.LedgerDuplicate {
			t.Errorf("endpoint %q: status = %s, want duplicate", ep, dup.Status)
		}
		if dup.ID != first.ID {
			t.Errorf("endpoint %q: dup.ID = %d, want first %d", ep, dup.ID, first.ID)
		}
	}
	// cwe 变体(cwe-89 / CWE89)→ 归一化后仍判重复
	for _, cweID := range []string{"cwe-89", "CWE89"} {
		dup, err := d.ReportVulnerability(ctx, "w-2", "easy-1", blackboard.VulnerabilityIn{
			CWE: cweID, Asset: "192.168.81.167:3000", Endpoint: "/rest/products/search?q=",
			Severity: "high", Title: "SQLi cwe variant",
		})
		if err != nil {
			t.Fatal(err)
		}
		if dup.Status != blackboard.LedgerDuplicate {
			t.Errorf("cwe %q: status = %s, want duplicate", cweID, dup.Status)
		}
	}
	// 不同 cwe → 不算重复
	other, err := d.ReportVulnerability(ctx, "w-2", "easy-1", blackboard.VulnerabilityIn{
		CWE: "CWE-79", Asset: "192.168.81.167:3000", Endpoint: "/rest/products/search?q=",
		Severity: "high", Title: "XSS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != blackboard.LedgerSubmitted {
		t.Errorf("不同 cwe: status = %s, want submitted", other.Status)
	}
	if n, _ := board.VulnerabilitiesLimit("easy-1", 0); len(n) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(n))
	}
	// 误报记录 → 方向可重试,不算重复
	if err := d.UpdateVulnerabilityReceipt(ctx, "w-1", "easy-1", first.ID, blackboard.LedgerFalsePositive, "fp-1"); err != nil {
		t.Fatal(err)
	}
	retry, err := d.ReportVulnerability(ctx, "w-3", "easy-1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "192.168.81.167:3000", Endpoint: "/rest/products/search?q=",
		Severity: "high", Title: "SQLi retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Status != blackboard.LedgerSubmitted {
		t.Errorf("false_positive 后重试: status = %s, want submitted", retry.Status)
	}
	// 跨 challenge 不误伤
	other, err = d.ReportVulnerability(ctx, "w-4", "other-1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "192.168.81.167:3000", Endpoint: "/rest/products/search?q=",
		Severity: "high", Title: "SQLi other task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != blackboard.LedgerSubmitted {
		t.Errorf("跨 challenge: status = %s, want submitted", other.Status)
	}
}

// ------------------------------------------------------------------ phase2 切片2:结构化归一化(03 §1.1)

// TestReportFindingsNormalized 验收:结构化字段归一化落库——
// cwe 大小写归一、asset 去协议/www、endpoint 去 query;语义重复被拦。
func TestReportFindingsNormalized(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, _ := board.SnapshotForWorker("easy-1")
	findings := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "登录口 SQL 注入", Weight: 0.8,
			CWE: "cwe-89", Asset: "https://www.Shop.Example.com/", Endpoint: "/login/?a=1", Severity: "high"},
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, findings); err != nil {
		t.Fatal(err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Fatalf("facts = %d", len(facts))
	}
	f := facts[0]
	if f.CWE != "CWE-89" || f.Asset != "shop.example.com" || f.Endpoint != "/login" || f.Severity != "high" {
		t.Fatalf("normalized fact = %+v", f)
	}
	// 语义重复(不同措辞、同结构化键)→ 幂等拦截
	snap2, _ := board.SnapshotForWorker("easy-1")
	findings2 := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "SQLi @ login 口", Weight: 0.7,
			CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high"},
	}
	if err := d.ReportFindings(context.Background(), "w-2", "easy-1", snap2.AsOfSeq, findings2); err != nil {
		t.Fatal(err)
	}
	facts, _ = board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Errorf("semantic duplicate not deduped: %d facts", len(facts))
	}
}

// TestReportFindingsCWEWhitelistReject 验收:白名单外 cwe → 拒字段不拒整条
// (finding 保留,退回逐字幂等),事件层提示。
func TestReportFindingsCWEWhitelistReject(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, _ := board.SnapshotForWorker("easy-1")
	findings := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "诡异漏洞类型", Weight: 0.8,
			CWE: "CWE-9999", Asset: "shop.example.com", Endpoint: "/x"},
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, findings); err != nil {
		t.Fatal(err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Fatalf("finding must not be dropped: %d facts", len(facts))
	}
	if facts[0].CWE != "" {
		t.Errorf("invalid cwe must be cleared, got %q", facts[0].CWE)
	}
	if facts[0].Asset != "shop.example.com" {
		t.Errorf("asset must survive field rejection: %q", facts[0].Asset)
	}
}

// TestReportFindingsSeverityWhitelist severity 白名单:非法值置空。
func TestReportFindingsSeverityWhitelist(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, _ := board.SnapshotForWorker("easy-1")
	findings := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "X", Weight: 0.5, CWE: "CWE-79",
			Asset: "a.example.com", Endpoint: "/s", Severity: "super-critical"},
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, findings); err != nil {
		t.Fatal(err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 1 || facts[0].Severity != "" {
		t.Fatalf("invalid severity must be cleared: %+v", facts)
	}
}

// TestReportIntentStructured 验收:intents 结构化键(target/approach)落库与防抖。
func TestReportIntentStructured(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	if err := d.ReportIntent(context.Background(), "w-1", "easy-1",
		IntentIn{Text: "对 /api/order 测试越权", Weight: 0.6, Target: "/api/order", Approach: "IDOR"}); err != nil {
		t.Fatal(err)
	}
	// 同键不同措辞 → 防抖(ErrNoChange 静默)
	if err := d.ReportIntent(context.Background(), "w-2", "easy-1",
		IntentIn{Text: "再测越权", Weight: 0.6, Target: "/api/order", Approach: "IDOR"}); err != nil {
		t.Fatalf("structured dup should be silent: %v", err)
	}
	intents, _ := board.Intents("easy-1", []string{blackboard.StateOpen}, 0)
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(intents))
	}
	if intents[0].Target != "/api/order" || intents[0].Approach != "IDOR" {
		t.Fatalf("structured intent = %+v", intents[0])
	}
}

// TestReportFindingsCWEInvalidStillValidatesSeverity 回归:白名单外 cwe 时
// severity 校验不得被短路(修复 normalizeFinding 提前 return)。
func TestReportFindingsCWEInvalidStillValidatesSeverity(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	snap, _ := board.SnapshotForWorker("easy-1")
	findings := []Finding{
		{Prefix: blackboard.PrefixVuln, Text: "X", Weight: 0.5,
			CWE: "CWE-9999", Asset: "a.example.com", Endpoint: "/x", Severity: "HIGH"},
	}
	if err := d.ReportFindings(context.Background(), "w-1", "easy-1", snap.AsOfSeq, findings); err != nil {
		t.Fatal(err)
	}
	facts, _ := board.Facts("easy-1", 0)
	if len(facts) != 1 {
		t.Fatalf("facts = %d", len(facts))
	}
	if facts[0].CWE != "" {
		t.Errorf("cwe must be cleared, got %q", facts[0].CWE)
	}
	if facts[0].Severity != "" {
		t.Errorf("invalid severity must be cleared even when cwe rejected, got %q", facts[0].Severity)
	}
}

// TestReportIntentTargetNormalized 回归:intent target 归一化(去尾部斜杠/query)。
func TestReportIntentTargetNormalized(t *testing.T) {
	d, board, _ := newTestDispatcher(t)
	if err := d.ReportIntent(context.Background(), "w-1", "easy-1",
		IntentIn{Text: "测越权", Weight: 0.6, Target: "/api/order/?a=1", Approach: "IDOR"}); err != nil {
		t.Fatal(err)
	}
	// 同归一化键不同原文 → 防抖
	if err := d.ReportIntent(context.Background(), "w-2", "easy-1",
		IntentIn{Text: "再测", Weight: 0.6, Target: "/api/order", Approach: "IDOR"}); err != nil {
		t.Fatalf("normalized target dup should be silent: %v", err)
	}
	intents, _ := board.Intents("easy-1", []string{blackboard.StateOpen}, 0)
	if len(intents) != 1 || intents[0].Target != "/api/order" {
		t.Fatalf("intents = %+v, want 1 with target /api/order", intents)
	}
}

// ------------------------------------------------------------------ phase2 切片3:攻击面清单(03 §2.2/§2.3)

// TestReportAttackSurfaceExpansion 验收:攻击面清单 → N 个初始 intent + 矩阵格子,
// 同 key 去重;带参数端点生成接力候选格。
func TestReportAttackSurfaceExpansion(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	d := New(board, nil, &fakeHooks{})
	cov := coverage.New(db)
	d.SetCoverage(cov)

	items := []AttackSurfaceItem{
		{Asset: "https://shop.example.com/", Endpoint: "/login", Params: []string{"user", "pass"}},
		{Asset: "shop.example.com", Endpoint: "/api/order", Params: []string{"id"}},
		{Asset: "shop.example.com", Endpoint: "/static/x.js"},
	}
	if err := d.ReportAttackSurface(context.Background(), "w-b", "easy-1", items); err != nil {
		t.Fatal(err)
	}
	// 3 个初始 intent(同 key 去重:归一化后 /login 与 /api/order 各一 + probe)
	intents, _ := board.Intents("easy-1", []string{blackboard.StateOpen}, 0)
	if len(intents) != 3 {
		t.Fatalf("intents = %d, want 3", len(intents))
	}
	// 矩阵格子:3 个资产格 + 带参端点的接力候选格(2 端点 × 3 CWE)
	cells, _ := cov.Cells("easy-1")
	if len(cells) < 3 {
		t.Fatalf("cells = %d, want >=3", len(cells))
	}
	// 幂等:重复落地不新增 intent
	if err := d.ReportAttackSurface(context.Background(), "w-b", "easy-1", items); err != nil {
		t.Fatal(err)
	}
	intents, _ = board.Intents("easy-1", []string{blackboard.StateOpen}, 0)
	if len(intents) != 3 {
		t.Errorf("re-report must be idempotent, intents = %d", len(intents))
	}
}

// TestReceiptAcceptedDrivesCoverage 验收:accepted 回执 → 矩阵格子 confirmed + 接力生成。
func TestReceiptAcceptedDrivesCoverage(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	d := New(board, nil, &fakeHooks{})
	cov := coverage.New(db)
	d.SetCoverage(cov)

	// 攻击面落地
	if err := d.ReportAttackSurface(context.Background(), "w-b", "easy-1",
		[]AttackSurfaceItem{{Asset: "shop.example.com", Endpoint: "/login", Params: []string{"u"}}}); err != nil {
		t.Fatal(err)
	}
	// 账本写入 + accepted 回执
	v, err := d.ReportVulnerability(context.Background(), "w-1", "easy-1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateVulnerabilityReceipt(context.Background(), "w-1", "easy-1", v.ID, blackboard.LedgerAccepted, "r1"); err != nil {
		t.Fatal(err)
	}
	// 矩阵:该格 confirmed
	cells, _ := cov.Cells("easy-1")
	foundConfirmed := false
	for _, c := range cells {
		if c.CWE == "CWE-89" && c.Endpoint == "/login" && c.Status == "confirmed" {
			foundConfirmed = true
		}
	}
	if !foundConfirmed {
		t.Fatalf("accepted receipt must confirm matrix cell: %+v", cells)
	}
	// 接力:同端点其他 CWE 候选格已生成(open)
	relay := false
	for _, c := range cells {
		if c.Endpoint == "/login" && c.Status == "open" && c.CWE != "" {
			relay = true
		}
	}
	if !relay {
		t.Errorf("relay cells missing after accepted: %+v", cells)
	}
}

// ------------------------------------------------------------------ phase2 切片4:平台 v2 回执闭环(04 §1/§2)

// TestRegisterHooksRoutesByChallenge 验收(阶段 2.19):并发多任务 hooks 按
// challengeID 路由(修复 SetHooks 单值覆盖——任务 A 派发串台到任务 B)。
func TestRegisterHooksRoutesByChallenge(t *testing.T) {
	d := New(nil, event.Discard, nil)
	hookA := &fakeHooks{}
	hookB := &fakeHooks{}
	d.RegisterHooks("task-A", hookA)
	d.RegisterHooks("task-B", hookB)

	// 未注册 challenge → 报错(不串台到默认/其他任务)
	if _, err := d.Launch(context.Background(), "synthesizer", Handoff{ChallengeID: "task-C"}); err == nil {
		t.Fatal("未注册 challenge 的 Launch 应报错")
	}

	// A/B 各自路由到自己的调度内核
	if _, err := d.Launch(context.Background(), "operator", Handoff{ChallengeID: "task-A"}); err != nil {
		t.Fatalf("task-A launch: %v", err)
	}
	if _, err := d.Launch(context.Background(), "synthesizer", Handoff{ChallengeID: "task-B"}); err != nil {
		t.Fatalf("task-B launch: %v", err)
	}
	if len(hookA.launched) != 1 || hookA.launched[0] != "operator" {
		t.Fatalf("hookA.launched = %v, want [operator]", hookA.launched)
	}
	if len(hookB.launched) != 1 || hookB.launched[0] != "synthesizer" {
		t.Fatalf("hookB.launched = %v, want [synthesizer](防串台)", hookB.launched)
	}

	// Steer 同路由(observer 按任务纠偏)
	if err := d.Steer(context.Background(), "task-B", "w-explore", Steer{TargetWorker: "w-explore", Message: "优先验证弱口令"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if len(hookA.steered) != 0 {
		t.Fatalf("hookA.steered = %v, want 0(steer 不应串台)", hookA.steered)
	}
	if len(hookB.steered) != 1 {
		t.Fatalf("hookB.steered = %v, want 1", hookB.steered)
	}

	// 注销后不再路由
	d.UnregisterHooks("task-B")
	if _, err := d.Launch(context.Background(), "synthesizer", Handoff{ChallengeID: "task-B"}); err == nil {
		t.Fatal("注销后 Launch 应报错")
	}
}

// TestAttackSurfaceParamsTolerant 验收(阶段 2.21):attack_surface.params 兼容
// 字符串("q")与数组(["q"])——真实 LLM 类型错误不再丢弃整条契约。
func TestAttackSurfaceParamsTolerant(t *testing.T) {
	for _, tc := range []struct {
		json string
		want []string
	}{
		{`{"asset":"a","endpoint":"/e","params":"q"}`, []string{"q"}},
		{`{"asset":"a","endpoint":"/e","params":["q","limit"]}`, []string{"q", "limit"}},
		{`{"asset":"a","endpoint":"/e"}`, nil},
	} {
		var it AttackSurfaceItem
		if err := json.Unmarshal([]byte(tc.json), &it); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.json, err)
		}
		if len(it.Params) != len(tc.want) {
			t.Fatalf("params = %v, want %v", it.Params, tc.want)
		}
		for i := range tc.want {
			if it.Params[i] != tc.want[i] {
				t.Fatalf("params = %v, want %v", it.Params, tc.want)
			}
		}
	}
	// 整条契约解析路径(嵌套 AttackSurfaceItem)
	c, err := ParseContract(`{"accepted":true,"attack_surface":[{"asset":"a","endpoint":"/e","params":"q"}]}`)
	if err != nil {
		t.Fatalf("ParseContract: %v", err)
	}
	if len(c.AttackSurface) != 1 || len(c.AttackSurface[0].Params) != 1 || c.AttackSurface[0].Params[0] != "q" {
		t.Fatalf("contract attack_surface = %+v", c.AttackSurface)
	}
}

// TestAttackSurfaceSingleObjectTolerant 验收:attack_surface 单对象
// (而非数组)不得让整条 Scout 契约 unparseable——实测真实 LLM 把单端点
// 输出为对象,导致 ReportAttackSurface 从未执行、覆盖矩阵为空。
func TestAttackSurfaceSingleObjectTolerant(t *testing.T) {
	c, err := ParseContract(`{"accepted":true,
		"attack_surface":{"asset":"192.168.81.167","endpoint":"/rest/products/search","params":"q","auth":"none","tech":"express","notes":"单端点"},
		"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`)
	if err != nil {
		t.Fatalf("ParseContract: %v", err)
	}
	if len(c.AttackSurface) != 1 {
		t.Fatalf("attack_surface = %d 条, want 1(单对象包装为数组)", len(c.AttackSurface))
	}
	if c.AttackSurface[0].Endpoint != "/rest/products/search" || len(c.AttackSurface[0].Params) != 1 || c.AttackSurface[0].Params[0] != "q" {
		t.Fatalf("attack_surface[0] = %+v", c.AttackSurface[0])
	}
}

// TestAttackSurfaceParamsObject 验收(阶段 2.21 补充):params 为对象
// ({"q":"描述"})与混合数组时宽容解析——实测真实 LLM 三种格式都输出过。
func TestAttackSurfaceParamsObject(t *testing.T) {
	c, err := ParseContract(`{"accepted":true,"attack_surface":[{"asset":"a","endpoint":"/e","params":{"q":"GET 查询参数"}},{"asset":"a","endpoint":"/f","params":["q",{"x":1}]}]}`)
	if err != nil {
		t.Fatalf("ParseContract: %v", err)
	}
	if len(c.AttackSurface) != 2 {
		t.Fatalf("attack_surface = %d 条, want 2(对象格式不得丢弃契约)", len(c.AttackSurface))
	}
	if len(c.AttackSurface[0].Params) != 1 || c.AttackSurface[0].Params[0] != "q" {
		t.Fatalf("对象 params = %v, want [q](取 keys)", c.AttackSurface[0].Params)
	}
}
