package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/report"
	"scopeforge/internal/store"
	"scopeforge/internal/testutil"
)

// testDeps 构建带完整组件的测试服务器。
func testDeps(t *testing.T, bc *event.Broadcaster) (*Server, *store.DB, *blackboard.Blackboard) {
	t.Helper()
	db := testutil.NewTestDB(t)
	cfgPath := filepath.Join(t.TempDir(), "scopeforge.yaml")
	_ = os.WriteFile(cfgPath, []byte("tools:\n  permissions: auto\n"), 0o644)
	board := blackboard.New(db)
	// 造数据
	_, _ = board.AddFact("c1", "vuln", "SQL 注入点", 0.9, "confirmed", "w1", "ev-2")
	_, _ = board.AddFact("c1", "obs", "登录口 /login", 0.7, "confirmed", "w1", "ev-2")
	// 账本记录(阶段 2.11:challenge 列表改从账本 distinct 读取;
	// 报告漏洞清单从账本引用证据,ev-2 对应 events 表 seq)
	_, _ = board.AddVulnerability("c1", blackboard.VulnerabilityIn{
		Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi", EvidenceRef: "ev-2",
	})
	sink := event.NewSQLiteSink(db)
	sink.Emit(event.Event{Kind: event.KindTurnStart, ChallengeID: "c1", SessionID: "s1"})
	sink.Emit(event.Event{Kind: event.KindToolCallStart, ChallengeID: "c1", SessionID: "s1",
		Payload: map[string]any{"id": "t1", "name": "bash"}})
	sink.Emit(event.Event{Kind: event.KindTraffic, ChallengeID: "c1", SessionID: "s1",
		Payload: map[string]any{"flow_ref": "flow-1", "method": "GET", "host": "t.example.com", "path": "/login", "status": 200}})

	srv := NewServer(Deps{
		DB:          db,
		Broadcaster: bc,
		Board:       board,
		Reports:     &report.Generator{DB: db, Board: board, WorkDir: t.TempDir()},
		ConfigPath:  cfgPath,
		AuthToken:   "secret-token",
	})
	return srv, db, board
}

func TestChallengesAndDetail(t *testing.T) {
	srv, _, _ := testDeps(t, nil)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/api/v1/challenges")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Challenges []map[string]any `json:"challenges"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Challenges) < 1 {
		t.Errorf("challenges=%d (账本 distinct challenge_id)", len(list.Challenges))
	}

	resp, err = http.Get(hs.URL + "/api/v1/challenges/c1")
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if detail["facts"] == nil {
		t.Errorf("detail=%+v", detail)
	}
	facts := detail["facts"].([]any)
	if len(facts) != 2 {
		t.Errorf("facts=%d", len(facts))
	}
}

func TestBoardAndEvidence(t *testing.T) {
	srv, _, _ := testDeps(t, nil)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/api/v1/challenges/c1/board")
	if err != nil {
		t.Fatal(err)
	}
	var board map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&board); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if board["facts"] == nil {
		t.Errorf("board=%+v", board)
	}

	// 证据溯源:ev-2(工具调用事件)+ flow-1(流量)
	for _, ref := range []string{"ev-2", "flow-1"} {
		resp, err := http.Get(hs.URL + "/api/v1/challenges/c1/evidence/" + ref)
		if err != nil {
			t.Fatal(err)
		}
		var ev map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if ev["event"] == nil {
			t.Errorf("evidence %s missing: %+v", ref, ev)
		}
		if ev["supporting_facts"] == nil {
			t.Errorf("supporting facts missing for %s", ref)
		}
	}
	// 伪造引用 404
	resp, _ = http.Get(hs.URL + "/api/v1/challenges/c1/evidence/ev-999")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("fake evidence status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSSEStreamBroadcast(t *testing.T) {
	bc := event.NewBroadcaster()
	srv, db, _ := testDeps(t, bc)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/api/v1/events/stream?after=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 实时发布事件 → SSE 应推送(必须经 BroadcastSink 才会广播)
	bs := event.NewBroadcastSink(db, bc)
	bs.Emit(event.Event{Kind: event.KindFinding, ChallengeID: "c1", SessionID: "s1",
		Payload: map[string]any{"text": "实时推送"}})

	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			got = append(got, line)
			if strings.Contains(line, "实时推送") {
				break // 实时广播事件已收到
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("SSE broadcast events=%d", len(got))
	}
	// 推送的事件确实来自广播器(含实时新事件)
	if !strings.Contains(strings.Join(got, ""), "实时推送") {
		t.Errorf("broadcast events missing new event: %v", got)
	}
}

func TestSystemEndpoint(t *testing.T) {
	srv, _, _ := testDeps(t, nil)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	resp, _ := http.Get(hs.URL + "/api/v1/system")
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["ts"] == nil {
		t.Errorf("system=%+v", body)
	}
}

func TestConfigEndpoints(t *testing.T) {
	srv, _, _ := testDeps(t, nil)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	resp, _ := http.Get(hs.URL + "/api/v1/config")
	var body struct {
		ConfigYAML string `json:"config_yaml"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if !strings.Contains(body.ConfigYAML, "permissions") {
		t.Errorf("config yaml missing: %q", body.ConfigYAML)
	}
	// PUT 校验:坏 yaml → 400;好 yaml → 200
	req, _ := http.NewRequest("PUT", hs.URL+"/api/v1/config", strings.NewReader(":: bad: [yaml"))
	req.Header.Set("Authorization", "Bearer secret-token")
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("bad config status=%d", resp2.StatusCode)
	}
	resp2.Body.Close()
	req3, _ := http.NewRequest("PUT", hs.URL+"/api/v1/config",
		strings.NewReader("providers:\n  - name: p\n    kind: openai\n    base_url: http://x\n    model: m\ntools:\n  permissions: auto\n"))
	req3.Header.Set("Authorization", "Bearer secret-token")
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3.StatusCode != 200 {
		t.Errorf("good config status=%d", resp3.StatusCode)
	}
	resp3.Body.Close()
	// 落盘校验(§1.2 全量 CRUD:保存生效)
	saved, err := os.ReadFile(srv.deps.ConfigPath)
	if err != nil || !strings.Contains(string(saved), "permissions: auto") {
		t.Errorf("config not persisted: %v %q", err, saved)
	}
}

func TestSPAFallback(t *testing.T) {
	srv, _, _ := testDeps(t, nil)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	resp, _ := http.Get(hs.URL + "/")
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	text := string(body[:n])
	if resp.StatusCode != 200 {
		t.Errorf("SPA status=%d", resp.StatusCode)
	}
	// 占位产物或真实产物都必须返回页面(embed 单二进制,§1.4)
	if !strings.Contains(text, "ScopeForge") {
		t.Errorf("SPA body=%s", text)
	}
}

var _ = fmt.Sprintf

func providerToolCall(name, args string) provider.ToolCall {
	return provider.ToolCall{ID: "tc1", Name: name, Arguments: args}
}
