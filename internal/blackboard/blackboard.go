// Package blackboard 是渗透编排的单一事实源(docs/03 §1)。
//
// 原则:
//   - SQLite 是唯一事实源;一切写入经 Dispatcher(本包只提供读写原语)。
//   - Fact 只增:update/delete 以 superseded_by 保留血缘(时间旅行)。
//   - 全局黑板 seq 单调递增;Worker 快照携带 asOfSeq,写入方以此检测冲突。
//   - 快照体积硬上限:facts ≤10 / intents ≤8(超限由 Observer 压缩)。
//   - 并发安全:本包非线程安全,所有方法调用必须串行化。
//     串行化由 SQLite 单连接(SetMaxOpenConns(1))保证。
//     若未来改为连接池或多 goroutine 写入,调用方必须在外部加锁。
package blackboard

import (
	"database/sql"
	"errors"
	"time"

	"scopeforge/internal/store"
)

// 前缀常量(docs/03 §1.2)。
const (
	PrefixObs  = "obs:"  // 观察
	PrefixVuln = "vuln:" // 漏洞
	PrefixFlag = "flag:" // 候选 flag
	PrefixHint = "hint:" // 提示(人工注入,最高权重)
	PrefixDead = "dead:" // 死路
	PrefixHyp  = "hyp:"  // 未验证假设
)

// 状态常量。
const (
	StateCandidate  = "candidate"
	StateConfirmed  = "confirmed"
	StateSuperseded = "superseded"
	StateFalsified  = "falsified"
	StateOpen       = "open"
	StateClaimed    = "claimed"
	StatePending    = "pending"
	StateDone       = "done"
	StateDead       = "dead"
)

// 快照硬裁剪上限(docs/03 §1.1.4)。
const (
	MaxFactsInSnapshot   = 10
	MaxIntentsInSnapshot = 8
)

// ErrNoChange 表示幂等写入(同内容已存在)。
var ErrNoChange = errors.New("blackboard: no change (duplicate)")

// Fact 是一条黑板事实。
type Fact struct {
	ID           int64   `json:"id"`
	Seq          int64   `json:"seq"`
	Prefix       string  `json:"prefix"`
	Text         string  `json:"text"`
	Weight       float64 `json:"weight"`
	State        string  `json:"state"`
	SupersededBy int64   `json:"superseded_by"` // 0 = 现行
	CreatedBy    string  `json:"created_by"`
	EvidenceRef  string  `json:"evidence_ref"`
	ChallengeID  string  `json:"challenge_id"`
	CreatedAt    int64   `json:"created_at"`
	// phase2 结构化键(docs/phase2/03 §1.1,全部可选):
	CWE      string `json:"cwe,omitempty"`
	Asset    string `json:"asset,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// Intent 是一个带权重探索方向。
type Intent struct {
	ID          int64   `json:"id"`
	Seq         int64   `json:"seq"`
	ChallengeID string  `json:"challenge_id"`
	Text        string  `json:"text"`
	Weight      float64 `json:"weight"`
	State       string  `json:"state"`
	ClaimedBy   string  `json:"claimed_by"`
	ClaimedAt   int64   `json:"claimed_at"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   int64   `json:"created_at"`
	// phase2 结构化键(docs/phase2/03 §1.3,可选):
	Target   string `json:"target,omitempty"`
	Approach string `json:"approach,omitempty"`
}

// Lease 是一个互斥租约。
type Lease struct {
	Resource  string
	Holder    string
	ExpiresAt int64
	CreatedAt int64
}

// 漏洞账本状态(docs/phase2/02 §1.3)。
const (
	LedgerSubmitted     = "submitted"
	LedgerAccepted      = "accepted"
	LedgerDuplicate     = "duplicate"
	LedgerFalsePositive = "false_positive"
	LedgerRejected      = "rejected"
)

// Vulnerability 是一条漏洞账本记录(只增不改,docs/phase2/02 §1)。
type Vulnerability struct {
	ID          int64  `json:"id"`
	Seq         int64  `json:"seq"`
	ChallengeID string `json:"challenge_id"`
	CWE         string `json:"cwe"`
	Asset       string `json:"asset"`
	Endpoint    string `json:"endpoint"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description"`
	EvidenceRef string `json:"evidence_ref"`
	PlatformRef string `json:"platform_ref"`
	Status      string `json:"status"`
	SubmittedAt int64  `json:"submitted_at"`
}

// VulnerabilityIn 是账本写入项。
type VulnerabilityIn struct {
	CWE         string
	Asset       string
	Endpoint    string
	Severity    string
	Title       string
	Description string
	EvidenceRef string
	PlatformRef string
}

// LedgerStats 是账本指标统计(recall/fpr 数据源,docs/phase2/02 §3.1)。
type LedgerStats struct {
	Total         int
	Accepted      int
	Duplicate     int
	FalsePositive int
	Rejected      int
	Submitted     int
}

// Worker 是调度内核的持久化心跳。
type Worker struct {
	ID                   string `json:"id"`
	ChallengeID          string `json:"challenge_id"`
	WorkerType           string `json:"worker_type"` // operator | synthesizer
	Phase                string `json:"phase"`        // operator 阶段: scout | executor | analyst(synthesizer 为空)
	Provider             string `json:"provider"`
	Model                string `json:"model"`
	SessionID            string `json:"session_id"`
	Status               string `json:"status"` // running | done | aborted | failed | orphaned
	Handoff              string `json:"handoff"`
	IntentID             int64  `json:"intent_id"`
	LastProgressAt       int64  `json:"last_progress_at"`
	HasCorrectSubmission bool   `json:"has_correct_submission"`
	CreatedAt            int64  `json:"created_at"`
	FinishedAt           int64  `json:"finished_at"`
}

// Snapshot 是注入 Worker 的黑板快照(限 YAML 增量文本)。
type Snapshot struct {
	Facts   []Fact   `json:"facts"`
	Intents []Intent `json:"intents"`
	AsOfSeq int64    `json:"as_of_seq"`
}

// BoardView 是调度器的态势视图(确定性决策输入)。
type BoardView struct {
	ChallengeID          string
	FactCount            int
	OpenIntents          []Intent // weight >= MinIntentWeight
	UnresolvedHypotheses int      // hyp: candidate
	DeadEnds             int      // dead: confirmed
}

// Blackboard 封装黑板读写(写入原语仅供 Dispatcher 使用)。
type Blackboard struct {
	db *store.DB
}

// New 构建黑板。
func New(db *store.DB) *Blackboard { return &Blackboard{db: db} }

// CurrentSeq 返回黑板全局序号。
func (b *Blackboard) CurrentSeq() (int64, error) {
	var v int64
	err := b.db.QueryRow(`SELECT value FROM blackboard_meta WHERE key='seq'`).Scan(&v)
	return v, err
}

// SnapshotForWorker 构建注入快照(asOfSeq + 体积硬裁剪)。
func (b *Blackboard) SnapshotForWorker(challengeID string) (*Snapshot, error) {
	facts, err := b.Facts(challengeID, MaxFactsInSnapshot)
	if err != nil {
		return nil, err
	}
	intents, err := b.Intents(challengeID, []string{StateOpen, StateClaimed}, MaxIntentsInSnapshot)
	if err != nil {
		return nil, err
	}
	seq, err := b.CurrentSeq()
	if err != nil {
		return nil, err
	}
	return &Snapshot{Facts: facts, Intents: intents, AsOfSeq: seq}, nil
}

// SnapshotForScheduler 构建调度态势视图(纯确定性)。
func (b *Blackboard) SnapshotForScheduler(challengeID string, minIntentWeight float64) (*BoardView, error) {
	view := &BoardView{ChallengeID: challengeID}
	facts, err := b.Facts(challengeID, 0)
	if err != nil {
		return nil, err
	}
	view.FactCount = len(facts)
	for _, f := range facts {
		switch f.Prefix {
		case PrefixHyp:
			if f.State == StateCandidate {
				view.UnresolvedHypotheses++
			}
		case PrefixDead:
			view.DeadEnds++
		}
	}
	intents, err := b.Intents(challengeID, []string{StateOpen, StateClaimed}, 0)
	if err != nil {
		return nil, err
	}
	for _, it := range intents {
		if it.Weight >= minIntentWeight && it.State == StateOpen {
			view.OpenIntents = append(view.OpenIntents, it)
		}
	}
	return view, nil
}

// FactIn 是批量写入项。
type FactIn struct {
	Prefix      string
	Text        string
	Weight      float64
	State       string
	CreatedBy   string
	EvidenceRef string
	// phase2 结构化键(可选;cwe/asset 归一化由 Dispatcher 完成):
	CWE      string
	Asset    string
	Endpoint string
	Severity string
}

// ErrSeqConflict 是黑板 seq 冲突(与 Dispatcher.ErrConflict 同语义,事务内检测)。
var ErrSeqConflict = errors.New("blackboard: seq conflict")

// AcquireLease 获取租约(resource 级互斥,过期自动让渡)。
// expires_at 以毫秒存储,支持亚秒 TTL(测试友好)。
func (b *Blackboard) AcquireLease(resource, holder string, ttl time.Duration) (bool, error) {
	now := time.Now().UnixMilli()
	expires := now + ttl.Milliseconds()
	tx, err := b.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var existing Lease
	err = tx.QueryRow(`SELECT resource, holder, expires_at FROM leases WHERE resource=?`, resource).
		Scan(&existing.Resource, &existing.Holder, &existing.ExpiresAt)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO leases (resource, holder, expires_at, created_at) VALUES (?, ?, ?, ?)`,
			resource, holder, expires, now); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case existing.ExpiresAt > now && existing.Holder != holder:
		return false, nil // 他人持有且未过期
	default:
		if _, err := tx.Exec(`UPDATE leases SET holder=?, expires_at=? WHERE resource=?`,
			holder, expires, resource); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// ReleaseLease 释放租约(仅持有者可释放)。
func (b *Blackboard) ReleaseLease(resource, holder string) error {
	_, err := b.db.Exec(`DELETE FROM leases WHERE resource=? AND holder=?`, resource, holder)
	return err
}

// ------------------------------------------------------------------ helpers

// nextSeq 在事务内推进黑板全局 seq。
func nextSeq(tx *sql.Tx) (int64, error) {
	var v int64
	if err := tx.QueryRow(`SELECT value FROM blackboard_meta WHERE key='seq'`).Scan(&v); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE blackboard_meta SET value=? WHERE key='seq'`, v+1); err != nil {
		return 0, err
	}
	return v + 1, nil
}

func withLimit(q string, limit int) string {
	if limit > 0 {
		return q + ` LIMIT ?`
	}
	return q
}

// limitArg 返回 LIMIT 参数(limit<=0 时无参数)。
func limitArg(limit int) []any {
	if limit > 0 {
		return []any{limit}
	}
	return nil
}

func statesClause(challengeID string, states []string) (string, []any) {
	if len(states) == 0 {
		return `challenge_id = ?`, []any{challengeID}
	}
	args := []any{challengeID}
	for _, s := range states {
		args = append(args, s)
	}
	return `challenge_id = ? AND state IN (` + placeholders(len(states)) + `)`, args
}

func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullIfEmpty 空字符串 → NULL(可空列)。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
