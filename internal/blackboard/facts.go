package blackboard

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ------------------------------------------------------------------ 读

// Facts 读取事实(confirmed 优先,weight 降序,limit<=0 时不限)。
// 包含 hyp: candidate(未验证假设,reason worker 的输入)。
func (b *Blackboard) Facts(challengeID string, limit int) ([]Fact, error) {
	q := `SELECT id, seq, prefix, text, weight, state, superseded_by, created_by, evidence_ref, challenge_id, created_at, cwe, asset, endpoint, severity
		FROM facts WHERE challenge_id = ? AND (state IN ('confirmed','superseded') OR (prefix='hyp:' AND state='candidate'))
		ORDER BY CASE WHEN state='confirmed' THEN 0 ELSE 1 END, weight DESC`
	rows, err := b.db.Query(withLimit(q, limit), append([]any{challengeID}, limitArg(limit)...)...)
	if err != nil {
		return nil, fmt.Errorf("blackboard: facts: %w", err)
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// FactByID 读取单条事实(任意状态)。
func (b *Blackboard) FactByID(id int64) (*Fact, error) {
	var f Fact
	var sup sql.NullInt64
	var cbS, evS, cweS, assetS, epS, sevS sql.NullString
	err := b.db.QueryRow(`SELECT id, seq, prefix, text, weight, state, superseded_by, created_by, evidence_ref, challenge_id, created_at, cwe, asset, endpoint, severity
		FROM facts WHERE id = ?`, id).
		Scan(&f.ID, &f.Seq, &f.Prefix, &f.Text, &f.Weight, &f.State, &sup, &cbS,
			&evS, &f.ChallengeID, &f.CreatedAt, &cweS, &assetS, &epS, &sevS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sup.Valid {
		f.SupersededBy = sup.Int64
	}
	f.CreatedBy = cbS.String
	f.EvidenceRef = evS.String
	f.CWE = cweS.String
	f.Asset = assetS.String
	f.Endpoint = epS.String
	f.Severity = sevS.String
	return &f, nil
}

// ------------------------------------------------------------------ 写(仅供 Dispatcher)

// AddFact 新增事实(幂等双轨:结构化键或逐字;同 challenge+prefix 已确认 → ErrNoChange)。
// state 默认 candidate;hint:/flag: 由调用方显式指定。
// 结构化字段可选:传 FactIn{...} 作为最后一个参数(切片 2,03 §1.1)。
func (b *Blackboard) AddFact(challengeID, prefix, text string, weight float64, state, createdBy, evidenceRef string, structured ...FactIn) (*Fact, error) {
	if state == "" {
		state = StateCandidate
	}
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	f, err := addFactTx(tx, challengeID, prefix, text, weight, state, createdBy, evidenceRef, structured...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return f, nil
}

// AddFactsAtomically 事务内批量写入 findings(seq 冲突检测与写入同事务,
// 防并发穿透 docs/03 §3.3)。asOfSeq<0 跳过冲突检测。
// 返回 ErrConflict 时整体未写入。
func (b *Blackboard) AddFactsAtomically(challengeID string, asOfSeq int64, items []FactIn) ([]Fact, error) {
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if asOfSeq >= 0 {
		var cur int64
		if err := tx.QueryRow(`SELECT value FROM blackboard_meta WHERE key='seq'`).Scan(&cur); err != nil {
			return nil, err
		}
		if cur > asOfSeq {
			return nil, ErrSeqConflict
		}
	}
	out := make([]Fact, 0, len(items))
	for _, it := range items {
		f, err := addFactTx(tx, challengeID, it.Prefix, it.Text, it.Weight, it.State, it.CreatedBy, it.EvidenceRef, it)
		if err != nil {
			if errors.Is(err, ErrNoChange) {
				continue // 幂等:NO_CHANGE
			}
			return nil, err
		}
		out = append(out, *f)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// SupersedeFact 标记旧事实被取代(update)或删除(deleteOnly)。
// update:旧行 superseded_by=新行 id(新行继承旧行文本或 newText)。
func (b *Blackboard) SupersedeFact(challengeID string, factID int64, newText string, weight float64, deleteOnly bool, createdBy string) error {
	old, err := b.FactByID(factID)
	if err != nil {
		return err
	}
	if old == nil {
		return fmt.Errorf("blackboard: fact %d not found", factID)
	}
	if old.ChallengeID != challengeID {
		return fmt.Errorf("blackboard: fact %d belongs to challenge %s", factID, old.ChallengeID)
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var newID int64
	if !deleteOnly {
		if newText == "" {
			newText = old.Text
		}
		if weight <= 0 {
			weight = old.Weight
		}
		seq, err := nextSeq(tx)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO facts (seq, prefix, text, weight, state, created_by, challenge_id, created_at, cwe, asset, endpoint, severity)
			VALUES (?, ?, ?, ?, 'confirmed', ?, ?, ?, ?, ?, ?, ?)`,
			seq, old.Prefix, newText, weight, createdBy, challengeID, time.Now().Unix(),
			nullIfEmpty(old.CWE), nullIfEmpty(old.Asset), nullIfEmpty(old.Endpoint), nullIfEmpty(old.Severity))
		if err != nil {
			return err
		}
		newID, _ = res.LastInsertId()
	} else {
		seq, err := nextSeq(tx)
		if err != nil {
			return err
		}
		_ = seq // delete 也占 seq,保持黑板全局推进
	}
	if _, err := tx.Exec(`UPDATE facts SET state='superseded', superseded_by=? WHERE id=?`, nullIfZero(newID), factID); err != nil {
		return err
	}
	return tx.Commit()
}

// addFactTx 事务内插入单条事实(幂等双轨 + seq 分配)。
// 幂等判定 v2(docs/phase2/03 §1.2):
//
//	轨 1:结构化键命中(challenge_id, prefix, cwe, asset, endpoint 归一化后一致)
//	轨 2:结构化键不全(cwe 或 asset 缺失)时退回逐字 (challenge_id, prefix, text)
func addFactTx(tx *sql.Tx, challengeID, prefix, text string, weight float64, state, createdBy, evidenceRef string, structured ...FactIn) (*Fact, error) {
	if state == "" {
		state = StateCandidate
	}
	var st FactIn
	if len(structured) > 0 {
		st = structured[0]
	}
	dup, err := dupFactID(tx, challengeID, prefix, text, state, st)
	if err != nil {
		return nil, err
	}
	if dup != 0 {
		return nil, ErrNoChange
	}
	seq, err := nextSeq(tx)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO facts (seq, prefix, text, weight, state, created_by, evidence_ref, challenge_id, created_at, cwe, asset, endpoint, severity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, prefix, text, weight, state, createdBy, evidenceRef, challengeID, time.Now().Unix(),
		nullIfEmpty(st.CWE), nullIfEmpty(st.Asset), nullIfEmpty(st.Endpoint), nullIfEmpty(st.Severity))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Fact{ID: id, Seq: seq, Prefix: prefix, Text: text, Weight: weight, State: state,
		CreatedBy: createdBy, EvidenceRef: evidenceRef, ChallengeID: challengeID,
		CWE: st.CWE, Asset: st.Asset, Endpoint: st.Endpoint, Severity: st.Severity}, nil
}

// dupFactID 幂等双轨判定,返回已存在的事实 id(0 = 无重复)。
func dupFactID(tx *sql.Tx, challengeID, prefix, text, state string, st FactIn) (int64, error) {
	stateCond := `(state='confirmed' OR (prefix='hyp:' AND state='candidate'))`
	// 轨 1:结构化键(cwe 与 asset 均非空才启用;endpoint 可空)
	if st.CWE != "" && st.Asset != "" {
		var dup int64
		err := tx.QueryRow(`SELECT id FROM facts WHERE challenge_id=? AND prefix=? AND cwe=? AND asset=? AND COALESCE(endpoint,'')=?
			AND `+stateCond, challengeID, prefix, st.CWE, st.Asset, st.Endpoint).Scan(&dup)
		if err == nil {
			return dup, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
		return 0, nil
	}
	// 轨 2:逐字(结构化键不全)
	var dup int64
	err := tx.QueryRow(`SELECT id FROM facts WHERE challenge_id=? AND prefix=? AND text=? AND `+stateCond,
		challengeID, prefix, text).Scan(&dup)
	if err == nil {
		return dup, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	return 0, nil
}

// scanFact 扫描一行 facts(含结构化列)。
func scanFact(s interface{ Scan(...any) error }) (*Fact, error) {
	var f Fact
	var sup sql.NullInt64
	var cbS, evS sql.NullString
	var cweS, assetS, epS, sevS sql.NullString
	err := s.Scan(&f.ID, &f.Seq, &f.Prefix, &f.Text, &f.Weight, &f.State, &sup, &cbS, &evS,
		&f.ChallengeID, &f.CreatedAt, &cweS, &assetS, &epS, &sevS)
	if err != nil {
		return nil, err
	}
	if sup.Valid {
		f.SupersededBy = sup.Int64
	}
	f.CreatedBy = cbS.String
	f.EvidenceRef = evS.String
	f.CWE = cweS.String
	f.Asset = assetS.String
	f.Endpoint = epS.String
	f.Severity = sevS.String
	return &f, nil
}
