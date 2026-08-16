package blackboard

import (
	"database/sql"
	"fmt"
	"time"
)

// ------------------------------------------------------------------ 漏洞账本(docs/phase2/02 §1)

// AddVulnerability 写入一条账本记录(只增不改,占黑板全局 seq)。
// severity 空时默认 info;status 固定为 submitted(回执状态经 UpdateVulnerabilityStatus 推进)。
func (b *Blackboard) AddVulnerability(challengeID string, v VulnerabilityIn) (*Vulnerability, error) {
	if v.Severity == "" {
		v.Severity = "info"
	}
	if v.Asset == "" {
		return nil, fmt.Errorf("blackboard: vulnerability asset required")
	}
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO vulnerability_ledger
		(seq, challenge_id, cwe, asset, endpoint, severity, title, description, evidence_ref, platform_ref, status, submitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'submitted', ?)`,
		seq, challengeID, nullIfEmpty(v.CWE), v.Asset, nullIfEmpty(v.Endpoint), v.Severity,
		v.Title, nullIfEmpty(v.Description), nullIfEmpty(v.EvidenceRef), nullIfEmpty(v.PlatformRef),
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Vulnerability{ID: id, Seq: seq, ChallengeID: challengeID, CWE: v.CWE, Asset: v.Asset,
		Endpoint: v.Endpoint, Severity: v.Severity, Title: v.Title, Description: v.Description,
		EvidenceRef: v.EvidenceRef, PlatformRef: v.PlatformRef, Status: LedgerSubmitted}, nil
}

// UpdateVulnerabilityStatus 推进回执状态(submitted → accepted/duplicate/false_positive/rejected)。
// 记录本身不删不改,仅 status/platform_ref 随平台回执更新(02 §1.3 提交纪律)。
func (b *Blackboard) UpdateVulnerabilityStatus(id int64, status, platformRef string) error {
	if status == "" {
		return fmt.Errorf("blackboard: vulnerability status required")
	}
	_, err := b.db.Exec(`UPDATE vulnerability_ledger SET status=?, platform_ref=COALESCE(?, platform_ref) WHERE id=?`,
		status, nullIfEmpty(platformRef), id)
	return err
}

// VulnerabilityByID 读取单条账本记录。
func (b *Blackboard) VulnerabilityByID(id int64) (*Vulnerability, error) {
	var v Vulnerability
	var cwe, ep, desc, ev, pr sql.NullString
	err := b.db.QueryRow(`SELECT id, seq, challenge_id, cwe, asset, endpoint, severity, title, description, evidence_ref, platform_ref, status, submitted_at
		FROM vulnerability_ledger WHERE id = ?`, id).
		Scan(&v.ID, &v.Seq, &v.ChallengeID, &cwe, &v.Asset, &ep, &v.Severity, &v.Title, &desc, &ev, &pr, &v.Status, &v.SubmittedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.CWE, v.Endpoint, v.Description, v.EvidenceRef, v.PlatformRef = cwe.String, ep.String, desc.String, ev.String, pr.String
	return &v, nil
}

// Vulnerabilities 读取账本全部记录(提交时间倒序)。
func (b *Blackboard) Vulnerabilities(challengeID string) ([]Vulnerability, error) {
	return b.vulnerabilities(challengeID, 0)
}

// VulnerabilitiesLimit 读取账本记录(limit 条,0=不限)。
func (b *Blackboard) VulnerabilitiesLimit(challengeID string, limit int) ([]Vulnerability, error) {
	return b.vulnerabilities(challengeID, limit)
}

// LastVulnerability 读取最近一条账本记录(无则 nil)。
func (b *Blackboard) LastVulnerability(challengeID string) (*Vulnerability, error) {
	vs, err := b.VulnerabilitiesLimit(challengeID, 1)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, nil
	}
	return &vs[0], nil
}

// LedgerStats 统计账本回执分布(评测指标数据源,docs/phase2/02 §3.1)。
func (b *Blackboard) LedgerStats(challengeID string) (LedgerStats, error) {
	var s LedgerStats
	rows, err := b.db.Query(`SELECT status, COUNT(*) FROM vulnerability_ledger WHERE challenge_id = ? GROUP BY status`, challengeID)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return s, err
		}
		s.Total += n
		switch st {
		case LedgerAccepted:
			s.Accepted = n
		case LedgerDuplicate:
			s.Duplicate = n
		case LedgerFalsePositive:
			s.FalsePositive = n
		case LedgerRejected:
			s.Rejected = n
		case LedgerSubmitted:
			s.Submitted = n
		}
	}
	return s, rows.Err()
}

func (b *Blackboard) vulnerabilities(challengeID string, limit int) ([]Vulnerability, error) {
	q := `SELECT id, seq, challenge_id, cwe, asset, endpoint, severity, title, description, evidence_ref, platform_ref, status, submitted_at
		FROM vulnerability_ledger WHERE challenge_id = ? ORDER BY submitted_at DESC, id DESC`
	args := []any{challengeID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("blackboard: vulnerabilities: %w", err)
	}
	defer rows.Close()
	var out []Vulnerability
	for rows.Next() {
		var v Vulnerability
		var cwe, ep, desc, ev, pr sql.NullString
		if err := rows.Scan(&v.ID, &v.Seq, &v.ChallengeID, &cwe, &v.Asset, &ep, &v.Severity, &v.Title, &desc, &ev, &pr, &v.Status, &v.SubmittedAt); err != nil {
			return nil, err
		}
		v.CWE, v.Endpoint, v.Description, v.EvidenceRef, v.PlatformRef = cwe.String, ep.String, desc.String, ev.String, pr.String
		out = append(out, v)
	}
	return out, rows.Err()
}
