package api

// GET /api/v1/tasks — 任务列表聚合端点(前端重构 06.11)。
//
// 数据源(聚合):events 表 run_started/run_done 为任务生命周期主轨,
// workers 表判定 challenge 模式"真实运行中"(调度器启动时清理崩溃
// 残留为 orphaned);vulnerability_ledger 提供漏洞计数。
// 覆盖全部任务(含零漏洞任务与 task 模式)——此前 /api/v1/challenges
// 只返回"至少提交过一条漏洞"的挑战,列表页永远看不到运行中任务。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"scopeforge/internal/event"
)

type taskCard struct {
	ID          string  `json:"id"`
	Mode        string  `json:"mode"`    // task | challenge
	Status      string  `json:"status"`  // running | done | failed | terminated
	Description string  `json:"description"` // 任务描述(首行),取自 run_started payload.task
	StartedAt   int64   `json:"started_at"`
	FinishedAt  int64   `json:"finished_at"`
	Turns       int     `json:"turns"`
	CostUSD     float64 `json:"cost_usd"`
	// 漏洞账本分组计数(替代旧的"vulns 全量计数"——submitted 未确认不算成果,
	// 避免列表页假数字;accepted = 确认成果,submitted = 待回执)
	Accepted      int `json:"accepted"`
	VulnSubmitted int `json:"vuln_submitted"`
	LastEventAt   int64 `json:"last_event_at"`
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	type runInfo struct {
		started  int64
		startSeq int64 // tie-break:同秒启动按事件 seq 排序
		mode     string
		task     string // 任务描述(task 模式)或题目标题(challenge 模式)
		finished int64
		status   string
		lastEv   int64
	}
	runs := map[string]*runInfo{}

	// 1. run_started:任务全集 + 开始时间 + mode
	if rows, err := s.deps.DB.Query(`SELECT challenge_id, MIN(ts), MIN(seq) FROM events WHERE kind = ? GROUP BY challenge_id`, event.KindRunStarted); err == nil {
		for rows.Next() {
			var id string
			var ts int64
			var seq int64
			if rows.Scan(&id, &ts, &seq) == nil {
				runs[id] = &runInfo{started: ts, startSeq: seq}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	} else {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 补 mode + description(payload 内)
	for id := range runs {
		var payload []byte
		_ = s.deps.DB.QueryRow(`SELECT payload FROM events WHERE challenge_id = ? AND kind = ? ORDER BY seq LIMIT 1`, id, event.KindRunStarted).Scan(&payload)
		if len(payload) > 0 {
			var p struct {
				Mode string `json:"mode"`
				Task string `json:"task"`
			}
			if json.Unmarshal(payload, &p) == nil {
				if p.Mode != "" {
					runs[id].mode = p.Mode
				}
				if p.Task != "" {
					runs[id].task = p.Task
				}
			}
		}
	}

	// 2. run_done:结束时间 + 状态(payload.error → failed;terminated → terminated)
	if rows, err := s.deps.DB.Query(`SELECT challenge_id, ts, payload FROM events WHERE kind = ?`, event.KindRunDone); err == nil {
		for rows.Next() {
			var id string
			var ts int64
			var payload []byte
			if rows.Scan(&id, &ts, &payload) == nil {
				if ri, ok := runs[id]; ok {
					ri.finished = ts
					var p struct {
						Error        string `json:"error"`
						Terminated   bool   `json:"terminated"`
						Interrupted  bool   `json:"interrupted"`
					}
					if json.Unmarshal(payload, &p) == nil {
						switch {
						case p.Interrupted:
							// 用户主动 stop(context canceled):显示"已停止",
							// 优先于 error 判定(interrupted 不是失败)。
							ri.status = "interrupted"
						case p.Error != "":
							ri.status = "failed"
						case p.Terminated:
							ri.status = "terminated"
						default:
							ri.status = "done"
						}
					}
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}

	// 3. 运行中判定(无 run_done):workers 表 running(调度器启动时会把崩溃
	// 残留清为 orphaned,challenge 模式以此为准);task 模式无 workers 记录,
	// 以 run_started 存在即 running(进程崩溃时 run_done 永不发出,前端
	// 观察到"运行中无进展"由用户决定重跑)。
	if rows, err := s.deps.DB.Query(`SELECT DISTINCT challenge_id FROM workers WHERE status = 'running'`); err == nil {
		var runningWorkers []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				runningWorkers = append(runningWorkers, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		rw := map[string]bool{}
		for _, id := range runningWorkers {
			rw[id] = true
		}
		for id, ri := range runs {
			if ri.finished == 0 && (rw[id] || ri.mode == "task") {
				ri.status = "running"
			}
		}
	}

	// 4. last_event_at + 僵尸判定
	// 进程重启后 run_done 丢失(goroutine 随进程消失,defer 未执行),
	// 任务会永远显示 running。超过 stale 窗口(10min)无新事件 → interrupted。
	staleCutoff := time.Now().Add(-10 * time.Minute).Unix()
	for id, ri := range runs {
		var ts sql.NullInt64
		_ = s.deps.DB.QueryRow(`SELECT MAX(ts) FROM events WHERE challenge_id = ?`, id).Scan(&ts)
		if ts.Valid {
			ri.lastEv = ts.Int64
		}
		// 兜底:无 run_done 且未命中任何判定(如进程崩溃残留)按 running 展示
		if ri.status == "" {
			ri.status = "running"
		}
		// 僵尸:running 但长期无事件(进程重启残留)
		if ri.status == "running" && ts.Valid && ts.Int64 > 0 && ts.Int64 < staleCutoff {
			ri.status = "interrupted"
		}
	}

	// 5. 装配卡(vulns/cost/turns)
	out := make([]taskCard, 0, len(runs))
	for id, ri := range runs {
		card := taskCard{
			ID: id, Mode: ri.mode, Status: ri.status,
			Description: firstLine(ri.task),
			StartedAt: ri.started, FinishedAt: ri.finished, LastEventAt: ri.lastEv,
		}
		if s.deps.Ledger != nil {
			if sp, err := s.deps.Ledger.Spend(id); err == nil {
				card.CostUSD = sp.CostUSD
				card.Turns = sp.Turns
			}
		}
		// 账本分组计数:accepted(确认成果)/ submitted(待回执)。其他状态
		// (duplicate/false_positive/rejected)不计数——不是成果也不是待办。
		_ = s.deps.DB.QueryRow(`SELECT COUNT(*) FROM vulnerability_ledger WHERE challenge_id=? AND status='accepted'`, id).Scan(&card.Accepted)
		_ = s.deps.DB.QueryRow(`SELECT COUNT(*) FROM vulnerability_ledger WHERE challenge_id=? AND status='submitted'`, id).Scan(&card.VulnSubmitted)
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt > out[j].StartedAt
		}
		return runs[out[i].ID].startSeq > runs[out[j].ID].startSeq
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":     out,
		"ts":        time.Now().Unix(),
		"generated": time.Now().Format(time.RFC3339),
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return strings.TrimSpace(s)
}
