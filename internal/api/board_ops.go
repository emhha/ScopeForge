package api

// 看板人工操作(2.25,2.18 遗留落地):
//   - intent 状态迁移(拖拽换列:计划↔已完成↔归档 = open/done/dead 互转)
//   - 覆盖格子写入(归档列真实写入路径:skip/dead + 归档回计划 reopen)
//
// 写操作经 checkpoint 事件落库 + 广播,前端事件驱动自动刷新(onBusEvent
// 监听 checkpoint → 1s 去抖 refresh),与调度器唯一写入者纪律互补——人工
// 操作是用户显式意图,不经过调度认领池。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
)

// POST /api/v1/challenges/{id}/board/intent/{intent_id}
// body: {"state": "open|done|dead|pending"}
// 拖拽换列映射:计划=open / 已完成=done / 归档=dead / pending(待条件)。
// claimed(进行中)是运行时状态,人工不可迁移——避免与调度认领竞争。
func (s *Server) handleBoardIntentState(w http.ResponseWriter, r *http.Request) {
	if s.deps.Board == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "board unavailable"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	challengeID := chi.URLParam(r, "id")
	intentID, err := strconv.ParseInt(chi.URLParam(r, "intent_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad intent_id"})
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	switch body.State {
	case blackboard.StateOpen, blackboard.StateDone, blackboard.StateDead, blackboard.StatePending:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("state %q invalid (open|done|dead|pending)", body.State)})
		return
	}
	// 查 intent:必须存在且属于该 challenge;claimed 不可人工迁移
	intents, err := s.deps.Board.Intents(challengeID, nil, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var it *blackboard.Intent
	for i := range intents {
		if intents[i].ID == intentID {
			it = &intents[i]
			break
		}
	}
	if it == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "intent not found"})
		return
	}
	if it.State == blackboard.StateClaimed {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "claimed intent 进行中不可手动迁移(worker 在跑)"})
		return
	}
	if err := s.deps.Board.UpdateIntentState(intentID, body.State, ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.emitBoardEvent(challengeID, "intent_state", map[string]any{
		"intent_id": intentID, "from": it.State, "to": body.State, "text": it.Text})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "intent_id": intentID, "state": body.State})
}

// POST /api/v1/challenges/{id}/board/cell
// body: {"cwe","asset","endpoint","action":"skip|dead|reopen","reason"}
//   - skip:  open/claimed → skipped(带理由,进报告)
//   - dead:  open/claimed → dead(方向排除)
//   - reopen: skipped/dead → open(清理由,回计划列)
//
// 与 challengeDetail 同表访问(coverage_matrix);RowsAffected==0 = 格子不存在
// 或当前状态不允许该迁移。
func (s *Server) handleBoardCellAction(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	challengeID := chi.URLParam(r, "id")
	var body struct {
		CWE      string `json:"cwe"`
		Asset    string `json:"asset"`
		Endpoint string `json:"endpoint"`
		Action   string `json:"action"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	if body.Asset == "" || body.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "asset/action required"})
		return
	}
	now := time.Now().Unix()
	var q string
	var args []any
	switch body.Action {
	case "skip":
		q = `UPDATE coverage_matrix SET status='skipped', skip_reason=?, updated_at=?
			WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND status IN ('open','claimed')`
		args = []any{body.Reason, now, challengeID, body.Asset, body.Endpoint}
	case "dead":
		q = `UPDATE coverage_matrix SET status='dead', skip_reason=?, updated_at=?
			WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND status IN ('open','claimed')`
		args = []any{body.Reason, now, challengeID, body.Asset, body.Endpoint}
	case "reopen":
		q = `UPDATE coverage_matrix SET status='open', skip_reason=NULL, updated_at=?
			WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND status IN ('skipped','dead')`
		args = []any{now, challengeID, body.Asset, body.Endpoint}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("action %q invalid (skip|dead|reopen)", body.Action)})
		return
	}
	res, err := s.deps.DB.Exec(q, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "格子不存在或当前状态不允许该迁移"})
		return
	}
	s.emitBoardEvent(challengeID, "cell_action", map[string]any{
		"cwe": body.CWE, "asset": body.Asset, "endpoint": body.Endpoint,
		"action": body.Action, "reason": body.Reason})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": body.Action})
}

// emitBoardEvent 落库 + 广播 checkpoint 事件(审计留痕 + 前端自动刷新)。
// DB 不可用时静默跳过(操作本身已生效,事件仅审计/刷新用途)。
func (s *Server) emitBoardEvent(challengeID, action string, detail map[string]any) {
	if s.deps.DB == nil {
		return
	}
	payload := map[string]any{"action": "board_" + action, "note": "人工看板操作", "detail": detail}
	if s.deps.Broadcaster != nil {
		event.NewBroadcastSink(s.deps.DB, s.deps.Broadcaster).Emit(event.Event{
			Kind: event.KindCheckpoint, ChallengeID: challengeID, Payload: payload,
		})
		return
	}
	event.NewSQLiteSink(s.deps.DB).Emit(event.Event{
		Kind: event.KindCheckpoint, ChallengeID: challengeID, Payload: payload,
	})
}
