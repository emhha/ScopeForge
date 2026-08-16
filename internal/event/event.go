// Package event 是类型化事件流:Agent 执行轨迹的唯一事实源
// (docs/02 §8.1)。所有事件落 events 表(seq 单调),SSE 按 seq 增量推送,
// 审计/回放/报告复用同一数据源。
package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"scopeforge/internal/store"
)

// Kind 是事件类型(docs/02 §8.1 契约)。
type Kind string

const (
	KindTurnStart         Kind = "turn_start"
	KindReasoningDelta    Kind = "reasoning_delta"
	KindTextDelta         Kind = "text_delta"
	KindToolCallStart     Kind = "tool_call_start"
	KindToolCallArgsDelta Kind = "tool_call_args_delta"
	KindToolCallResult    Kind = "tool_call_result"
	KindUsage             Kind = "usage"
	KindSubmission        Kind = "submission"
	KindFinding           Kind = "finding"
	KindSteer             Kind = "steer"
	KindCheckpoint        Kind = "checkpoint"
	KindError             Kind = "error"
	// M1 编排事件
	KindWorkerLaunch Kind = "worker_launch"
	KindWorkerAbort  Kind = "worker_abort"
	KindWorkerDone   Kind = "worker_done"
	KindTick         Kind = "scheduler_tick"
	KindTermination  Kind = "termination"
	KindBoardChange  Kind = "board_change"
	// M2 能力层事件(docs/04)
	KindDenied   Kind = "denied"      // 确定性 Hook 拦截(命令/外泄/目标)
	KindTraffic  Kind = "traffic"     // 流量审计(flowRef/method/host/path/status)
	KindRoute    Kind = "route"       // 隧道生命周期(open/stop/reconnect/status)
	KindListener Kind = "listener"    // 反连监听器(open/close/connection)
	KindUnlock   Kind = "tool_unlock" // 动态池解锁
	KindKBSearch Kind = "kb_search"   // 漏洞知识库检索
	// M3 观测与交互事件(docs/05)
	KindApproval Kind = "approval" // HITL 审批(pending/approved/denied/modified/suspended/timeout)
	KindReport   Kind = "report"   // 报告生成
	KindReview   Kind = "review"   // 漏洞人工复核(pending/accept/reject/false_positive,05 §4)
	// M3.5 Web 发起任务(serve 内异步 run)
	KindRunStarted Kind = "run_started" // 任务启动(web 入口)
	KindRunDone    Kind = "run_done"    // 任务结束(成功/失败摘要)
)

// Event 是一条事件。
type Event struct {
	Seq         int64
	SeqEnd      int64 `json:"seq_end,omitempty"` // 合并事件块的结束 seq(原始单事件为 0)
	TS          int64
	Kind        Kind
	Payload     any
	SessionID   string
	ChallengeID string
	TraceID     string // 分布式追踪 ID(scheduler tick 生成,worker 继承)
	Level       Level  // 日志级别(默认 Info)
}

// Level 是事件严重级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Sink 是事件接收器。
type Sink interface {
	Emit(e Event)
}

// FuncSink 适配函数。
type FuncSink func(Event)

func (f FuncSink) Emit(e Event) { f(e) }

// Discard 丢弃一切。
var Discard Sink = FuncSink(func(Event) {})

// FanOut 分发到多个 sink。
type FanOut []Sink

func (f FanOut) Emit(e Event) {
	for _, s := range f {
		s.Emit(e)
	}
}

// SQLiteSink 把事件写入 events 表。
type SQLiteSink struct {
	db *store.DB
}

// NewSQLiteSink 构建落库 sink。
func NewSQLiteSink(db *store.DB) *SQLiteSink { return &SQLiteSink{db: db} }

func (s *SQLiteSink) Emit(e Event) {
	_, _ = s.emitWithSeq(e)
}

// emitWithSeq 落库并返回 DB 分配的 seq(同连接事务,保证 last_insert_rowid 正确)。
// 广播器依赖回填的 seq 做单调去重(docs/05 §2.1)。
func (s *SQLiteSink) emitWithSeq(e Event) (int64, error) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	var payload []byte
	if e.Payload != nil {
		payload, _ = json.Marshal(e.Payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO events (ts, kind, payload, session_id, challenge_id, trace_id, level) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.TS, string(e.Kind), payload, e.SessionID, e.ChallengeID, e.TraceID, string(e.Level))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// Query 读取事件:按 session 过滤(空 = 全部),seq > after 增量拉取。
// Query 查询事件:after(seq > after 升序)或 before(seq < before 降序,翻页用)。
func Query(db *store.DB, sessionID string, after, before int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}
	var rows *sql.Rows
	var err error
	if before > 0 {
		rows, err = db.Query(`SELECT seq, ts, kind, payload, session_id, challenge_id, COALESCE(trace_id,''), COALESCE(level,'info') FROM events
			WHERE seq < ? AND (? = '' OR session_id = ?) ORDER BY seq DESC LIMIT ?`,
			before, sessionID, sessionID, limit)
	} else {
		rows, err = db.Query(`SELECT seq, ts, kind, payload, session_id, challenge_id, COALESCE(trace_id,''), COALESCE(level,'info') FROM events
			WHERE seq > ? AND (? = '' OR session_id = ?) ORDER BY seq ASC LIMIT ?`,
			after, sessionID, sessionID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.TS, &e.Kind, &payload, &e.SessionID, &e.ChallengeID, &e.TraceID, &e.Level); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			var v any
			if json.Unmarshal(payload, &v) == nil {
				e.Payload = v
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MergeDeltas 把逐字增量事件聚合为块,用于历史 REST 响应压缩:
// 数据库按审计粒度逐 delta 存储(SSE 实时需要),但翻页接口不需要把
// 500 条 "_re" 一类的事件全部推给前端。同 session、同 kind、seq 相邻
// (gap ≤ 200)的 reasoning/text/tool_call_args 合并,Payload.text 为拼接全文,
// Seq 保留块首 seq,SeqEnd 为块尾 seq;其它事件原样保留。
func MergeDeltas(events []Event) []Event {
	if len(events) < 2 {
		return events
	}
	desc := events[0].Seq > events[1].Seq
	asc := events
	if desc {
		asc = make([]Event, len(events))
		for i := range events {
			asc[len(events)-1-i] = events[i]
		}
	}
	merged := mergeDeltasAsc(asc)
	if !desc {
		return merged
	}
	out := make([]Event, len(merged))
	for i := range merged {
		out[len(merged)-1-i] = merged[i]
	}
	return out
}

func mergeDeltasAsc(events []Event) []Event {
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if !isDeltaKind(e.Kind) {
			out = append(out, e)
			continue
		}
		text := deltaText(e.Payload)
		if n := len(out); n > 0 && isDeltaKind(out[n-1].Kind) &&
			out[n-1].SessionID == e.SessionID && e.Seq-out[n-1].SeqEnd <= 200 {
			last := &out[n-1]
			prev := deltaText(last.Payload)
			last.Payload = map[string]any{"text": prev + text}
			last.SeqEnd = e.Seq
			continue
		}
		cp := e
		cp.SeqEnd = e.Seq
		cp.Payload = map[string]any{"text": text}
		out = append(out, cp)
	}
	return out
}

func isDeltaKind(k Kind) bool {
	return k == KindReasoningDelta || k == KindTextDelta || k == KindToolCallArgsDelta
}

func deltaText(p any) string {
	if m, ok := p.(map[string]any); ok {
		for _, key := range []string{"text", "delta", "arg_chars", "args"} {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// 避免全表内存过滤)。
func QueryForChallenge(db *store.DB, challengeID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100000
	}
	rows, err := db.Query(`SELECT seq, ts, kind, payload, session_id, challenge_id, COALESCE(trace_id,''), COALESCE(level,'info') FROM events
		WHERE challenge_id = ? ORDER BY seq ASC LIMIT ?`, challengeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.TS, &e.Kind, &payload, &e.SessionID, &e.ChallengeID, &e.TraceID, &e.Level); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			var v any
			if json.Unmarshal(payload, &v) == nil {
				e.Payload = v
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestSeq 返回最新事件序号(增量拉取的游标)。
func LatestSeq(db *store.DB) (int64, error) {
	var seq int64
	err := db.QueryRow(`SELECT COALESCE(max(seq), 0) FROM events`).Scan(&seq)
	return seq, err
}
