package api

// /api/v1/tasks 任务聚合端点测试(06.11):run_started/run_done 生命周期、
// 运行中判定、状态推导、漏洞计数。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
	"scopeforge/internal/testutil"
)

func TestTasksAggregation(t *testing.T) {
	db := testutil.NewTestDB(t)
	sink := event.NewSQLiteSink(db)

	// 任务 A:challenge 模式,已 terminated
	sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: "juice-a", Payload: map[string]any{"mode": "challenge"}})
	sink.Emit(event.Event{Kind: event.KindWorkerLaunch, ChallengeID: "juice-a", Payload: map[string]any{"worker": "w1", "type": "operator"}})
	sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: "juice-a", Payload: map[string]any{"mode": "challenge", "terminated": true, "reason": "no_more_work"}})
	// 任务 B:task 模式,运行中(无 run_done)
	sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: "task-1", Payload: map[string]any{"mode": "task"}})
	sink.Emit(event.Event{Kind: event.KindTurnStart, SessionID: "s1"}) // 无 challenge_id,聚合不依赖它
	// 任务 C:失败(failed)
	sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: "juice-c", Payload: map[string]any{"mode": "challenge"}})
	sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: "juice-c", Payload: map[string]any{"mode": "challenge", "error": "provider down"}})
	// 任务 D:用户 stop(context canceled → interrupted,非 failed)
	sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: "task-d", Payload: map[string]any{"mode": "task"}})
	sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: "task-d", Payload: map[string]any{"mode": "task", "interrupted": true}})

	srv := httptest.NewServer(NewServer(Deps{DB: db}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Tasks []taskCard `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 4 {
		t.Fatalf("tasks = %d, want 4", len(out.Tasks))
	}
	byID := map[string]taskCard{}
	for _, c := range out.Tasks {
		byID[c.ID] = c
	}
	if byID["juice-a"].Status != "terminated" || byID["juice-a"].Mode != "challenge" || byID["juice-a"].FinishedAt == 0 {
		t.Errorf("juice-a = %+v", byID["juice-a"])
	}
	if byID["task-1"].Status != "running" || byID["task-1"].Mode != "task" || byID["task-1"].FinishedAt != 0 {
		t.Errorf("task-1 = %+v", byID["task-1"])
	}
	if byID["juice-c"].Status != "failed" {
		t.Errorf("juice-c = %+v", byID["juice-c"])
	}
	if byID["task-d"].Status != "interrupted" {
		t.Errorf("task-d(用户 stop)= %+v, want interrupted(非 failed)", byID["task-d"])
	}
	// 排序:started_at 降序(后写的任务在前)
	if out.Tasks[0].ID != "task-d" || out.Tasks[1].ID != "juice-c" || out.Tasks[3].ID != "juice-a" {
		t.Errorf("order = %s,%s,%s,%s", out.Tasks[0].ID, out.Tasks[1].ID, out.Tasks[2].ID, out.Tasks[3].ID)
	}
}

// TestTasksVulnStatusCounts 验收:列表页漏洞计数按状态分组——
// accepted(确认成果)与 submitted(待回执)分开,duplicate/false_positive 不计。
func TestTasksVulnStatusCounts(t *testing.T) {
	db := testutil.NewTestDB(t)
	sink := event.NewSQLiteSink(db)
	sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: "vuln-a", Payload: map[string]any{"mode": "challenge"}})
	sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: "vuln-a", Payload: map[string]any{"mode": "challenge"}})

	board := blackboard.New(db)
	v1, _ := board.AddVulnerability("vuln-a", blackboard.VulnerabilityIn{CWE: "CWE-89", Asset: "a.com", Title: "SQLi"})
	v2, _ := board.AddVulnerability("vuln-a", blackboard.VulnerabilityIn{CWE: "CWE-79", Asset: "a.com", Title: "XSS"})
	v3, _ := board.AddVulnerability("vuln-a", blackboard.VulnerabilityIn{CWE: "CWE-22", Asset: "a.com", Title: "LFI"})
	_ = board.UpdateVulnerabilityStatus(v1.ID, blackboard.LedgerAccepted, "r1")
	_ = board.UpdateVulnerabilityStatus(v2.ID, blackboard.LedgerAccepted, "r2")
	_ = board.UpdateVulnerabilityStatus(v3.ID, blackboard.LedgerFalsePositive, "fp")

	srv := httptest.NewServer(NewServer(Deps{DB: db, Board: board}).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Tasks []taskCard `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(out.Tasks))
	}
	c := out.Tasks[0]
	if c.Accepted != 2 {
		t.Errorf("accepted = %d, want 2", c.Accepted)
	}
	if c.VulnSubmitted != 0 {
		t.Errorf("vuln_submitted = %d, want 0(false_positive 不计)", c.VulnSubmitted)
	}
	// 第三条 false_positive 不产生 submitted 计数
}
