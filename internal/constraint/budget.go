package constraint

import (
	"fmt"
	"time"

	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/store"
)

// Spend 是单题累计消耗。
type Spend struct {
	PromptTokens     int64
	CacheHitTokens   int64
	OutputTokens     int64
	ReasoningTokens  int64
	CostUSD          float64
	Turns            int
}

// CostLedger 按 role×model 记账(每 Usage 事件一行,docs/03 §6.1)。
type CostLedger struct {
	db *store.DB
}

// NewCostLedger 构建账本。
func NewCostLedger(db *store.DB) *CostLedger { return &CostLedger{db: db} }

// Record 记一笔 usage。pricing 为 nil 时 cost 记 0。
func (l *CostLedger) Record(sessionID, challengeID, role, model string, u *provider.Usage, p *provider.Pricing) error {
	cost := 0.0
	if p != nil && u != nil {
		cost = p.Cost(u)
	}
	pt, ct, ot, rt := 0, 0, 0, 0
	if u != nil {
		pt, ct, ot, rt = u.PromptTokens, u.CacheHitTokens, u.CompletionTokens, u.ReasoningTokens
	}
	_, err := l.db.Exec(`INSERT INTO ledger (session_id, challenge_id, role, model, prompt_tokens, cache_hit_tokens, output_tokens, reasoning_tokens, cost_usd, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, challengeID, role, model, pt, ct, ot, rt, cost, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("constraint: ledger record: %w", err)
	}
	return nil
}

// Spend 汇总单题消耗(按 sessions.challenge_id 关联)。
func (l *CostLedger) Spend(challengeID string) (Spend, error) {
	var s Spend
	err := l.db.QueryRow(`SELECT
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cache_hit_tokens),0),
		COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
		COALESCE(SUM(cost_usd),0), COUNT(*)
		FROM ledger WHERE challenge_id = ?`, challengeID).
		Scan(&s.PromptTokens, &s.CacheHitTokens, &s.OutputTokens, &s.ReasoningTokens, &s.CostUSD, &s.Turns)
	if err != nil {
		return s, fmt.Errorf("constraint: ledger spend: %w", err)
	}
	return s, nil
}

// ------------------------------------------------------------------ BudgetMeter

// Budget 是单题预算(docs/03 §6.2)。
type Budget struct {
	MaxTokensPerChallenge int64   // 0 = 不限
	MaxTurnsPerChallenge  int     // 0 = 不限
	MaxCostUSD            float64 // 0 = 不限
}

// Meter 是三维熔断器。
type Meter struct {
	db     *store.DB
	budget Budget
}

// NewMeter 构建熔断器。
func NewMeter(db *store.DB, budget Budget) *Meter { return &Meter{db: db, budget: budget} }

// Check 判定是否仍允许继续。allowed=false 时 reason 说明熔断维度。
func (m *Meter) Check(challengeID string) (bool, string) {
	s, err := m.Spend(challengeID)
	if err != nil {
		return false, "ledger unavailable: " + err.Error()
	}
	return m.checkSpend(s)
}

// CheckSpend 直接按给定消耗判定(测试/预测用)。
func (m *Meter) checkSpend(s Spend) (bool, string) {
	if m.budget.MaxTokensPerChallenge > 0 && s.PromptTokens+s.CacheHitTokens >= m.budget.MaxTokensPerChallenge {
		return false, "tokens"
	}
	if m.budget.MaxTurnsPerChallenge > 0 && s.Turns >= m.budget.MaxTurnsPerChallenge {
		return false, "turns"
	}
	if m.budget.MaxCostUSD > 0 && s.CostUSD >= m.budget.MaxCostUSD {
		return false, "cost"
	}
	return true, ""
}

// Spend 委托账本。
func (m *Meter) Spend(challengeID string) (Spend, error) {
	l := CostLedger{db: m.db}
	return l.Spend(challengeID)
}
