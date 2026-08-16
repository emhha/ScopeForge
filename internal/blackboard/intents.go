package blackboard

import (
	"database/sql"
	"fmt"
	"time"
)

// Intents 读取意图(指定状态,weight 降序)。
func (b *Blackboard) Intents(challengeID string, states []string, limit int) ([]Intent, error) {
	where, args := statesClause(challengeID, states)
	q := `SELECT id, seq, challenge_id, text, weight, state, claimed_by, claimed_at, created_by, created_at, target, approach
		FROM intents WHERE ` + where + ` ORDER BY weight DESC`
	rows, err := b.db.Query(withLimit(q, limit), append(args, limitArg(limit)...)...)
	if err != nil {
		return nil, fmt.Errorf("blackboard: intents: %w", err)
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		var it Intent
		var cb sql.NullString
		var cat sql.NullInt64
		var tg, ap sql.NullString
		if err := rows.Scan(&it.ID, &it.Seq, &it.ChallengeID, &it.Text, &it.Weight, &it.State, &cb, &cat, &it.CreatedBy, &it.CreatedAt, &tg, &ap); err != nil {
			return nil, err
		}
		if cb.Valid {
			it.ClaimedBy = cb.String
		}
		if cat.Valid {
			it.ClaimedAt = cat.Int64
		}
		it.Target = tg.String
		it.Approach = ap.String
		out = append(out, it)
	}
	return out, rows.Err()
}

// AddIntent 新增意图(weight 越高越优先)。
// 防抖双轨(docs/phase2/03 §1.3):target+approach 都提供时按结构化键,
// 否则退回逐字文本。
func (b *Blackboard) AddIntent(challengeID, text string, weight float64, createdBy string, structured ...IntentIn) (*Intent, error) {
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 防抖:同键 open intent 已存在 → ErrNoChange
	var dup int64
	var it IntentIn
	if len(structured) > 0 {
		it = structured[0]
	}
	if it.Target != "" && it.Approach != "" {
		err = tx.QueryRow(`SELECT id FROM intents WHERE challenge_id=? AND target=? AND approach=? AND state IN ('open','claimed','pending')`,
			challengeID, it.Target, it.Approach).Scan(&dup)
	} else {
		err = tx.QueryRow(`SELECT id FROM intents WHERE challenge_id=? AND text=? AND state IN ('open','claimed','pending')`,
			challengeID, text).Scan(&dup)
	}
	if err == nil {
		return nil, ErrNoChange
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	seq, err := nextSeq(tx)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO intents (seq, challenge_id, text, weight, state, created_by, created_at, target, approach)
		VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?)`,
		seq, challengeID, text, weight, createdBy, time.Now().Unix(),
		nullIfEmpty(it.Target), nullIfEmpty(it.Approach))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Intent{ID: id, Seq: seq, ChallengeID: challengeID, Text: text, Weight: weight,
		State: StateOpen, CreatedBy: createdBy, Target: it.Target, Approach: it.Approach}, nil
}

// IntentIn 是意图写入的结构化扩展(可选 target/approach)。
type IntentIn struct {
	Target   string
	Approach string
}

// UpdateIntentState 更新意图状态(open → claimed/pending/done/dead)。
// 认领(claimed)时要求当前状态为 open——防重复认领竞态。
func (b *Blackboard) UpdateIntentState(id int64, state, claimedBy string) error {
	var claimedAt any
	if state == StateClaimed || state == StatePending {
		claimedAt = time.Now().Unix()
	}
	where := `id=?`
	if state == StateClaimed {
		where += ` AND state='open'` // 仅 open→claimed 需要原子性
	}
	res, err := b.db.Exec(`UPDATE intents SET state=?, claimed_by=?, claimed_at=COALESCE(?, claimed_at) WHERE `+where,
		state, claimedBy, claimedAt, id)
	if err != nil {
		return err
	}
	if state == StateClaimed {
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("blackboard: intent %d already claimed or not open", id)
		}
	}
	return nil
}
