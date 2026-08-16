package api

// 看板人工操作 API 测试(2.25):intent 状态迁移白名单/claimed 拒绝/格子
// skip|dead|reopen/事件落库可查。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
	"scopeforge/internal/testutil"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func boardTestServer(t *testing.T, withToken bool) (*httptest.Server, *blackboard.Blackboard) {
	t.Helper()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	tok := ""
	if withToken {
		tok = "tok-1"
	}
	srv := NewServer(Deps{DB: db, Board: board, AuthToken: tok, Broadcaster: event.NewBroadcaster()})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, board
}

func postBoard(t *testing.T, url, tok, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestBoardIntentState 验收:intent 状态迁移(拖拽换列)——open→done→dead 互转,
// claimed 拒绝,非法 state 拒绝,未授权拒绝。
func TestBoardIntentState(t *testing.T) {
	hs, board := boardTestServer(t, true)
	// 预置三个 intent:open / claimed / dead
	it1, err := board.AddIntent("c1", "测 /login SQLi", 0.7, "scheduler", blackboard.IntentIn{})
	if err != nil {
		t.Fatal(err)
	}
	it2, err := board.AddIntent("c1", "测 /admin 越权", 0.6, "scheduler", blackboard.IntentIn{})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.UpdateIntentState(it2.ID, blackboard.StateClaimed, "w-9"); err != nil {
		t.Fatal(err)
	}
	it3, err := board.AddIntent("c1", "测 /api 注入", 0.5, "scheduler", blackboard.IntentIn{})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.UpdateIntentState(it3.ID, blackboard.StateDead, ""); err != nil {
		t.Fatal(err)
	}

	// open → done(拖到已完成列)
	code, out := postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it1.ID), "tok-1", `{"state":"done"}`)
	if code != 200 {
		t.Fatalf("open→done = %d %v", code, out)
	}
	// done → dead(已完成 → 归档)
	code, out = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it1.ID), "tok-1", `{"state":"dead"}`)
	if code != 200 {
		t.Fatalf("done→dead = %d %v", code, out)
	}
	// dead → open(归档 → 计划)
	code, out = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it3.ID), "tok-1", `{"state":"open"}`)
	if code != 200 {
		t.Fatalf("dead→open = %d %v", code, out)
	}
	// claimed 拒绝(进行中不可手动迁移)
	code, out = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it2.ID), "tok-1", `{"state":"dead"}`)
	if code != http.StatusConflict {
		t.Fatalf("claimed→dead = %d, want 409 (%v)", code, out)
	}
	// 非法 state
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it3.ID), "tok-1", `{"state":"claimed"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("claimed state = %d, want 400", code)
	}
	// 未授权
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it3.ID), "", `{"state":"dead"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	// 不存在 intent
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/99999", "tok-1", `{"state":"dead"}`)
	if code != http.StatusNotFound {
		t.Fatalf("missing intent = %d, want 404", code)
	}

	// 最终状态落库校验
	intents, _ := board.Intents("c1", nil, 0)
	state := map[int64]string{}
	for i := range intents {
		state[intents[i].ID] = intents[i].State
	}
	if state[it1.ID] != blackboard.StateDead || state[it3.ID] != blackboard.StateOpen || state[it2.ID] != blackboard.StateClaimed {
		t.Errorf("final states = %v", state)
	}
}

// TestBoardCellAction 验收:格子 skip(带理由)/dead/reopen;不存在或状态不
// 允许 → 409;reopen 清 skip_reason。
func TestBoardCellAction(t *testing.T) {
	db := testutil.NewTestDB(t)
	// 预置格子:open + skipped(带理由)+ 其他 challenge
	if _, err := db.Exec(`INSERT INTO coverage_matrix (challenge_id, cwe, asset, endpoint, status, form, created_at, updated_at) VALUES ('c1','CWE-89','shop.example.com','/login','open','form',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO coverage_matrix (challenge_id, cwe, asset, endpoint, status, form, skip_reason, created_at, updated_at) VALUES ('c1','CWE-22','shop.example.com','/download','skipped','form','穷尽排除',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO coverage_matrix (challenge_id, cwe, asset, endpoint, status, form, created_at, updated_at) VALUES ('c2','CWE-89','other.example.com','/x','open','form',1,1)`); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(Deps{DB: db, AuthToken: "tok-1"})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	// open → skip(带理由,归档列写入)
	code, out := postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "tok-1",
		`{"cwe":"CWE-89","asset":"shop.example.com","endpoint":"/login","action":"skip","reason":"人工排除"}`)
	if code != 200 {
		t.Fatalf("skip = %d %v", code, out)
	}
	// skip → reopen(回计划列,清理由)
	code, out = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "tok-1",
		`{"cwe":"CWE-22","asset":"shop.example.com","endpoint":"/download","action":"reopen"}`)
	if code != 200 {
		t.Fatalf("reopen = %d %v", code, out)
	}
	// 已 skipped 的格子再 skip → 409(状态不允许)
	code, out = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "tok-1",
		`{"cwe":"CWE-89","asset":"shop.example.com","endpoint":"/login","action":"skip"}`)
	if code != http.StatusConflict {
		t.Fatalf("re-skip = %d, want 409 (%v)", code, out)
	}
	// 不存在的格子 → 409
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "tok-1",
		`{"cwe":"CWE-89","asset":"nope.example.com","endpoint":"/x","action":"reopen"}`)
	if code != http.StatusConflict {
		t.Fatalf("missing cell = %d, want 409", code)
	}
	// 非法 action
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "tok-1",
		`{"cwe":"CWE-89","asset":"shop.example.com","endpoint":"/login","action":"confirm"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad action = %d, want 400", code)
	}
	// 未授权
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c1/board/cell", "",
		`{"cwe":"CWE-89","asset":"shop.example.com","endpoint":"/login","action":"dead"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	// 其他 challenge 不受影响
	code, _ = postBoard(t, hs.URL+"/api/v1/challenges/c2/board/cell", "tok-1",
		`{"cwe":"CWE-89","asset":"other.example.com","endpoint":"/x","action":"skip"}`)
	if code != 200 {
		t.Fatalf("c2 skip = %d", code)
	}

	// 落库校验:skip_reason 写入与 reopen 清空
	var status, reason string
	err := db.QueryRow(`SELECT status, COALESCE(skip_reason,'') FROM coverage_matrix WHERE challenge_id='c1' AND asset='shop.example.com' AND endpoint='/login'`).Scan(&status, &reason)
	if err != nil || status != "skipped" || reason != "人工排除" {
		t.Errorf("cell1 = %s %q (%v)", status, reason, err)
	}
	err = db.QueryRow(`SELECT status, COALESCE(skip_reason,'') FROM coverage_matrix WHERE challenge_id='c1' AND asset='shop.example.com' AND endpoint='/download'`).Scan(&status, &reason)
	if err != nil || status != "open" || reason != "" {
		t.Errorf("cell2 = %s %q (%v)", status, reason, err)
	}
}

// TestBoardActionEmitsCheckpoint 验收:操作落 checkpoint 事件(审计 + 前端
// 事件驱动刷新通道)。
func TestBoardActionEmitsCheckpoint(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	it, _ := board.AddIntent("c1", "测 /x", 0.7, "scheduler", blackboard.IntentIn{})
	srv := NewServer(Deps{DB: db, Board: board, AuthToken: "tok-1", Broadcaster: event.NewBroadcaster()})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	code, _ := postBoard(t, hs.URL+"/api/v1/challenges/c1/board/intent/"+itoa(it.ID), "tok-1", `{"state":"dead"}`)
	if code != 200 {
		t.Fatalf("intent move = %d", code)
	}
	evs, err := event.Query(db, "", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		if e.Kind == event.KindCheckpoint && e.ChallengeID == "c1" {
			if p, ok := e.Payload.(map[string]any); ok && p["action"] == "board_intent_state" {
				found = true
			}
		}
	}
	if !found {
		t.Error("checkpoint 事件未落库(board_intent_state)")
	}
}
