// Package api 是 HTTP API 服务器(docs/05 §1):chi 路由 + SSE 广播事件流 +
// REST(challenges/board/evidence/ledger/system/config)
// + WS PTY 终端 + go:embed SPA。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
	"scopeforge/internal/event"
	"scopeforge/internal/kb"
	"scopeforge/internal/listener"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/report"
	"scopeforge/internal/route"
	"scopeforge/internal/sandbox"
	"scopeforge/internal/store"
	"scopeforge/internal/tmux"
)

// Deps 是 API 服务器依赖(各组件可 nil;端点返回 503)。
type Deps struct {
	DB          *store.DB
	Broadcaster *event.Broadcaster
	Board       *blackboard.Blackboard
	Ledger      *constraint.CostLedger
	Meter       *constraint.Meter
	Tmux        *tmux.Manager
	Route       *route.Manager
	Listener    *listener.Manager
	KB          *kb.Index
	Sandbox     *sandbox.Docker
	Providers   map[string]provider.Provider
	Cfg         config.Config
	Reports     *report.Generator
	WorkDir     string
	ConfigPath  string // 配置文件路径(PUT /api/v1/config 落盘用)
	AuthToken   string // 写操作(审批决策/终端写/未脱敏报告)的 bearer token;空 = 不校验
	Runner      Runner // Web 发起任务(task/challenge 异步启动);nil = 未装配
}

// Server 是 HTTP API 服务器。
type Server struct {
	deps Deps
	mux  *chi.Mux
}

// NewServer 构建 API 服务器(docs/05 §1.1 拓扑)。
func NewServer(deps Deps) *Server {
	s := &Server{deps: deps}
	r := chi.NewRouter()
	// 系统
	r.Get("/health", s.handleHealth)
	// 事件流(唯一实时通道)
	r.Get("/api/v1/events", s.handleEvents)
	r.Get("/api/v1/events/stream", s.handleEventsStream)
	r.Get("/api/v1/sessions", s.handleSessions)
	// REST
	r.Get("/api/v1/challenges", s.handleChallenges)
	r.Get("/api/v1/tasks", s.handleTasks)
	r.Get("/api/v1/challenges/{id}", s.handleChallengeDetail)
	r.Get("/api/v1/challenges/{id}/board", s.handleBoard)
	r.Get("/api/v1/challenges/{id}/evidence/{ref}", s.handleEvidence)
	// 看板人工操作(2.25):intent 状态迁移(拖拽换列)+ 格子 skip/dead/reopen
	r.Post("/api/v1/challenges/{id}/board/intent/{intent_id}", s.handleBoardIntentState)
	r.Post("/api/v1/challenges/{id}/board/cell", s.handleBoardCellAction)
	r.Get("/api/v1/ledger", s.handleLedger)
	r.Get("/api/v1/system", s.handleSystem)
	r.Get("/api/v1/config", s.handleGetConfig)
	r.Put("/api/v1/config", s.handlePutConfig)
	// Web 发起任务(M3.5)
	r.Post("/api/v1/run", s.handleRun)
	r.Post("/api/v1/runs/{id}/stop", s.handleRunStop)
	// WS 终端
	r.Get("/ws/pty", s.handlePTY)
	// SPA(go:embed;构建产物缺失时返回提示)
	r.Get("/", s.handleSPA)
	r.Get("/*", s.handleSPA)
	s.mux = r
	return s
}

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

// ------------------------------------------------------------------ 基础

// health 响应。
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Time    int64  `json:"time"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil || s.deps.DB.Ping() != nil {
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "degraded", Version: version})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: version, Time: time.Now().Unix()})
}

// handleEvents 增量拉取:?after=<seq>&before=<seq>&session=<id>&limit=<n>
// (断线续拉 + 时间线向前翻页,§2.1)。after/before 互斥,before 优先。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	session := r.URL.Query().Get("session")
	events, err := event.Query(s.deps.DB, session, after, before, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rawCount := len(events)
	if r.URL.Query().Get("merge") == "1" {
		events = event.MergeDeltas(events)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":    events,
		"latest":    mustLatest(s.deps.DB),
		"raw_count": rawCount,
	})
}

// handleEventsStream 是 SSE 推送端点(M3 广播器,替代 500ms 轮询):
// 先订阅广播器(防丢),再补拉 after 之后的历史(去重:seq<=after 跳过),随后实时推送。
// 断线后客户端带 ?after=<last_seq> 重连 → 续拉无重复无丢失(§2.5)。
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	session := r.URL.Query().Get("session")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	// 先订阅再补拉:订阅后到达的事件进 chan,补拉结果按 seq 去重
	ch, cancel := s.subscribeOrNil()
	if cancel != nil {
		defer cancel()
	}
	writeEvent := func(e event.Event) bool {
		if session != "" && e.SessionID != session {
			return false
		}
		if e.Seq <= after {
			return false // 去重
		}
		data, err := json.Marshal(e)
		if err != nil {
			return false
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		after = e.Seq
		return true
	}
	// 补拉历史(断线续拉)
	if s.deps.DB != nil {
		if evs, err := event.Query(s.deps.DB, session, after, 0, 1000); err == nil {
			for _, e := range evs {
				writeEvent(e)
			}
		}
	}
	if len(ch) > 0 {
		// 消费订阅窗口内事件(去重由 writeEvent 保证)
		for {
			select {
			case e := <-ch:
				writeEvent(e)
			default:
				goto drainDone
			}
		}
	}
drainDone:
	fl.Flush()
	ctx := r.Context()
	if ch == nil {
		if s.deps.DB == nil {
			// 无广播器且无 DB:只能挂起到连接关闭
			<-ctx.Done()
			return
		}
		// 无广播器(测试/降级):500ms 轮询兜底
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if evs, err := event.Query(s.deps.DB, session, after, 0, 100); err == nil {
					for _, e := range evs {
						writeEvent(e)
					}
					if len(evs) > 0 {
						fl.Flush()
					}
				}
			}
		}
	}
	// 心跳:25s 事件帧保活(§1.4 自动重连;慢消费者被广播器断开后,
	// 前端靠"无数据超时"主动重连,after 续拉无丢失)
	// 用 data: 事件帧而非注释行(: ping)——EventSource 不暴露注释行,
	// 前端无数据计时器(45s)无法被注释心跳重置,空闲任务会每 45s 误重连。
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, "data: {\"hb\":1}\n\n")
			fl.Flush()
		case e := <-ch:
			if writeEvent(e) {
				fl.Flush()
			}
		}
	}
}

// subscribeOrNil 订阅广播器(不存在返回 nil,nil)。
func (s *Server) subscribeOrNil() (<-chan event.Event, func()) {
	if s.deps.Broadcaster == nil {
		return nil, nil
	}
	return s.deps.Broadcaster.Subscribe()
}

// handleSessions 列出会话。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	rows, err := s.deps.DB.Query(`SELECT id, kind, provider, model, rewrite_version, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	type sessionRow struct {
		ID             string `json:"id"`
		Kind           string `json:"kind"`
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		RewriteVersion int    `json:"rewrite_version"`
		UpdatedAt      int64  `json:"updated_at"`
	}
	var out []sessionRow
	for rows.Next() {
		var sr sessionRow
		if err := rows.Scan(&sr.ID, &sr.Kind, &sr.Provider, &sr.Model, &sr.RewriteVersion, &sr.UpdatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out = append(out, sr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func mustLatest(db *store.DB) int64 {
	seq, err := event.LatestSeq(db)
	if err != nil {
		return 0
	}
	return seq
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// version 由 -ldflags 注入。
var version = "0.3.0-m3"
