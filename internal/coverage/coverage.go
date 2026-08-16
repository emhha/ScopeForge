// Package coverage 是覆盖矩阵(docs/phase2/03 §3.1)——全面性核心。
//
// 格子 = (CWE × Asset × Endpoint) 的一个探索单元,状态机:
//
//	open(攻击面落地/接力生成) → claimed(explore 认领)
//	                             → confirmed(漏洞 accepted)
//	                             → dead(方向耗尽)
//	                             → skipped(穷尽声明排除,带理由)
//
// 职责:
//   - 矩阵读写(持久化 coverage_matrix 表,唯一键 challenge+cwe+asset+endpoint)
//   - IsConverged:coverage 形态终止判定(02 §2.3,实现 constraint.ConvergenceEvaluator)
//   - 接力候选生成(03 §3.4 第 1 层:端点形态 → 受限 CWE 子集,非全组合)
//
// 写入者纪律:仅 Dispatcher/Scheduler 经本包写,格子状态是调度的确定性输入。
package coverage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"scopeforge/internal/constraint"
	"scopeforge/internal/store"
)

// 格子状态(docs/phase2/03 §3.1)。
const (
	StatusOpen      = "open"
	StatusClaimed   = "claimed"
	StatusConfirmed = "confirmed"
	StatusDead      = "dead"
	StatusSkipped   = "skipped"
)

// 端点形态(03 §3.4 第 1 层候选规则的分类依据)。
const (
	FormUnknown = "unknown"
	FormParam   = "param"  // 带参数端点
	FormFile    = "file"   // 文件类端点
	FormUpload  = "upload" // 上传类端点
	FormAuth    = "auth"   // 认证入口
	FormStatic  = "static" // 静态资源(不生成注入类接力)
)

// Cell 是覆盖矩阵的一个格子。
type Cell struct {
	ID         int64  `json:"id"`
	Challenge  string `json:"challenge_id"`
	CWE        string `json:"cwe"`
	Asset      string `json:"asset"`
	Endpoint   string `json:"endpoint"`
	Status     string `json:"status"`
	Form       string `json:"form"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// Matrix 是覆盖矩阵(读写原语,经 db 持久化)。
type Matrix struct {
	db *store.DB
}

// New 构建覆盖矩阵。
func New(db *store.DB) *Matrix { return &Matrix{db: db} }

// ------------------------------------------------------------------ 读写

// EnsureOpen 建立 open 格子(幂等:同键已存在则不动)。
// cwe 可空(攻击面落地时"未指定");form 记录端点形态供接力生成。
func (m *Matrix) EnsureOpen(challengeID, cwe, asset, endpoint, form string) error {
	if asset == "" {
		return fmt.Errorf("coverage: asset required")
	}
	now := time.Now().Unix()
	_, err := m.db.Exec(`INSERT OR IGNORE INTO coverage_matrix
		(challenge_id, cwe, asset, endpoint, status, form, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'open', ?, ?, ?)`,
		challengeID, nullIfEmpty(cwe), asset, nullIfEmpty(endpoint), form, now, now)
	return err
}

// MarkClaimed 端点级认领 → claimed(explore 认领方向,03 §3.1)。
// 只标记该端点的"未指定 CWE"攻击面格;asset 可空 = 按 endpoint 跨资产匹配
// (调度 intent 只携带 target,不携带 asset)。
func (m *Matrix) MarkClaimed(challengeID, asset, endpoint string) error {
	q := `UPDATE coverage_matrix SET status='claimed', updated_at=?
		WHERE challenge_id=? AND (cwe IS NULL OR cwe='') AND status='open'`
	args := []any{time.Now().Unix(), challengeID}
	if asset != "" {
		q += ` AND asset=?`
		args = append(args, asset)
	}
	if endpoint != "" {
		q += ` AND endpoint=?`
		args = append(args, endpoint)
	}
	_, err := m.db.Exec(q, args...)
	return err
}

// MarkClaimedCell 精确格子认领(补格 intent 派活时,带 cwe)。
func (m *Matrix) MarkClaimedCell(challengeID, cwe, asset, endpoint string) error {
	_, err := m.db.Exec(`UPDATE coverage_matrix SET status='claimed', updated_at=?
		WHERE challenge_id=? AND cwe=? AND asset=? AND endpoint=? AND status='open'`,
		time.Now().Unix(), challengeID, cwe, asset, endpoint)
	return err
}

// MarkConfirmed 格子 → confirmed(漏洞 accepted,账本回执驱动)。
// 命中规则:优先精确 (cwe,asset,endpoint);endpoint 空时按 (cwe,asset) 或 asset 级。
// 若攻击面清单未成功落地导致格子不存在,则确定性补建后确认——覆盖矩阵
// 不因 Scout 契约格式失败而永久缺失已确认漏洞的格子。
func (m *Matrix) MarkConfirmed(challengeID, cwe, asset, endpoint string) error {
	now := time.Now().Unix()
	if endpoint != "" && cwe != "" {
		res, err := m.db.Exec(`UPDATE coverage_matrix SET status='confirmed', updated_at=?
			WHERE challenge_id=? AND cwe=? AND asset=? AND endpoint=?`,
			now, challengeID, cwe, asset, endpoint)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			form := m.cellForm(challengeID, asset, endpoint)
			if form == "" {
				form = ClassifyForm(endpoint)
			}
			if err := m.EnsureOpen(challengeID, cwe, asset, endpoint, form); err != nil {
				return err
			}
			_, err = m.db.Exec(`UPDATE coverage_matrix SET status='confirmed', updated_at=?
				WHERE challenge_id=? AND cwe=? AND asset=? AND endpoint=?`,
				now, challengeID, cwe, asset, endpoint)
		}
		return err
	}
	// 未精确命中:同 asset 全部确认?保守:只确认同 (asset) 的"未指定 CWE"格,
	// 精确格留给调度认领后的回执(带 cwe)。无此资产级格时补建后确认。
	if asset != "" {
		if err := m.EnsureOpen(challengeID, "", asset, "", ""); err != nil {
			return err
		}
	}
	_, err := m.db.Exec(`UPDATE coverage_matrix SET status='confirmed', updated_at=?
		WHERE challenge_id=? AND asset=? AND (cwe IS NULL OR cwe='')`,
		now, challengeID, asset)
	return err
}

// MarkDead 端点级方向耗尽 → dead(intent exhausted;asset 可空 = 按 endpoint 匹配)。
func (m *Matrix) MarkDead(challengeID, asset, endpoint string) error {
	q := `UPDATE coverage_matrix SET status='dead', updated_at=?
		WHERE challenge_id=? AND status IN ('open','claimed')`
	args := []any{time.Now().Unix(), challengeID}
	if asset != "" {
		q += ` AND asset=?`
		args = append(args, asset)
	}
	if endpoint != "" {
		q += ` AND endpoint=?`
		args = append(args, endpoint)
	}
	_, err := m.db.Exec(q, args...)
	return err
}

// MarkSkipped 格子 → skipped(穷尽声明排除,带理由进报告)。
func (m *Matrix) MarkSkipped(challengeID, asset, endpoint, reason string) error {
	_, err := m.db.Exec(`UPDATE coverage_matrix SET status='skipped', skip_reason=?, updated_at=?
		WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND status IN ('open','claimed')`,
		reason, time.Now().Unix(), challengeID, asset, endpoint)
	return err
}

// ReopenSkippedAuth 重开因"缺登录态"跳过的格子(获取凭据后重测,登录后攻击面闭环)。
// 只匹配 skip_reason 含登录类关键词的 skipped 格子——穷尽声明排除(无登录理由)
// 不受影响,防把"已排除方向"无谓复活。asset 空 = 跨资产全部匹配。
// 返回重新打开的格子数;重开格子回到 open,调度经补格 intent 自动续派 explore。
func (m *Matrix) ReopenSkippedAuth(challengeID, asset string) (int64, error) {
	q := `UPDATE coverage_matrix SET status='open', skip_reason=NULL, updated_at=?
		WHERE challenge_id=? AND status='skipped' AND (
			skip_reason LIKE '%登录%' OR skip_reason LIKE '%login%'
			OR skip_reason LIKE '%认证%' OR skip_reason LIKE '%auth%')`
	args := []any{time.Now().Unix(), challengeID}
	if asset != "" {
		q += ` AND asset=?`
		args = append(args, asset)
	}
	res, err := m.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Cells 读取全部格子(challenge 维度)。
func (m *Matrix) Cells(challengeID string) ([]Cell, error) {
	rows, err := m.db.Query(`SELECT id, challenge_id, cwe, asset, endpoint, status, form, skip_reason
		FROM coverage_matrix WHERE challenge_id=? ORDER BY id ASC`, challengeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cell
	for rows.Next() {
		var c Cell
		var cwe, ep, form, skip sql.NullString
		if err := rows.Scan(&c.ID, &c.Challenge, &cwe, &c.Asset, &ep, &c.Status, &form, &skip); err != nil {
			return nil, err
		}
		c.CWE, c.Endpoint, c.Form, c.SkipReason = cwe.String, ep.String, form.String, skip.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenCells 读取 open 状态格子(补格 intent 数据源)。
func (m *Matrix) OpenCells(challengeID string) ([]Cell, error) {
	cells, err := m.Cells(challengeID)
	if err != nil {
		return nil, err
	}
	var out []Cell
	for _, c := range cells {
		if c.Status == StatusOpen {
			out = append(out, c)
		}
	}
	return out, nil
}

// IsConverged 实现 constraint.ConvergenceEvaluator(docs/phase2/02 §2.3):
// 收敛 = 每个 interest 命中的已知格子都处于终态(confirmed|dead|claimed|skipped)。
// 攻击面都没测绘出来(无格子)→ 不算收敛;open 格子存在 → 未收敛。
func (m *Matrix) IsConverged(ctx context.Context, challengeID string, interest constraint.Interest) (bool, string) {
	cells, err := m.Cells(challengeID)
	if err != nil {
		return false, "coverage_error: " + err.Error()
	}
	if len(cells) == 0 {
		return false, "no_attack_surface" // 攻击面都没测绘出来,不算收敛
	}
	for _, cell := range cells {
		// interest 过滤:客户不感兴趣的格子不参与收敛判定。
		// 注意:空 CWE 的攻击面格(端点级)永远参与——它们代表端点探索本身,
		// 不能被 CWE 集合过滤跳过,否则全 open 也会误判收敛(零探索提前终止)。
		if cell.CWE != "" && !interest.Match(cell.CWE, "") {
			continue
		}
		if cell.Status == StatusOpen {
			return false, fmt.Sprintf("open_cell: %s@%s%s", cell.CWE, cell.Asset, cell.Endpoint)
		}
	}
	return true, "coverage_converged"
}

// IsFocusedCellConfirmed 聚焦任务完成判定(08 §3 缺失④):
// 聚焦模式下只有1个目标格子(GenerateRelay 被抑制),任意格子 confirmed 即视为任务完成。
// 实现 constraint.FocusedTaskEvaluator。
func (m *Matrix) IsFocusedCellConfirmed(ctx context.Context, challengeID string, interest constraint.Interest) (bool, string) {
	cells, err := m.Cells(challengeID)
	if err != nil {
		return false, "focus_error: " + err.Error()
	}
	for _, c := range cells {
		if c.Status == StatusConfirmed {
			return true, "focused_cell_confirmed:" + c.CWE + "@" + c.Asset + c.Endpoint
		}
	}
	return false, ""
}

// ------------------------------------------------------------------ 接力候选生成(03 §3.4 第 1 层)

// CandidateCWEs 按端点形态返回受限候选 CWE 子集(每端点 ≤ 3-5 个,非全组合):
//
//	带参数 → {SQLi, XSS, IDOR}
//	文件类 → {路径穿越, 任意文件读}
//	上传类 → {文件上传, 存储型 XSS}
//	认证入口 → {弱口令, 会话管理, CSRF}
//	静态资源 → 空(不生成注入类接力)
//	未知 → 空(形态未知先 probe,不猜,03 §3.4)
func CandidateCWEs(form string) []string {
	switch form {
	case FormParam:
		return []string{"CWE-89", "CWE-79", "CWE-639"}
	case FormFile:
		return []string{"CWE-22", "CWE-200"}
	case FormUpload:
		return []string{"CWE-434", "CWE-79"}
	case FormAuth:
		return []string{"CWE-307", "CWE-287", "CWE-352"}
	default: // unknown / static
		return nil
	}
}

// ClassifyForm 推断端点形态(关键词匹配,03 §3.4 端点形态 → 候选 CWE 子集)。
func ClassifyForm(endpoint string) string {
	e := strings.ToLower(endpoint)
	switch {
	case strings.Contains(e, "upload"):
		return FormUpload
	case strings.Contains(e, "download") || strings.Contains(e, "file") || strings.Contains(e, "static"):
		return FormFile
	case strings.Contains(e, "login") || strings.Contains(e, "auth") || strings.Contains(e, "signin") || strings.Contains(e, "password"):
		return FormAuth
	default:
		return FormUnknown
	}
}

// GenerateRelay 生成相邻漏洞接力候选(03 §3.4):
//   - 同端点其他 CWE:按端点形态的候选子集,排除已 confirmed 的 CWE
//   - 同 CWE 其他端点:仅对"同形态"端点展开(注入类不接力到静态资源)
//
// 端点形态以矩阵落地时确定的 form 为准(Dispatcher 按 params 判定,
// 带参端点即使路径叫 /login 也是 FormParam);无记录时回退 ClassifyForm。
// 候选只进矩阵 open 格子(EnsureOpen 幂等),不直接派活——派活由调度认领。
func (m *Matrix) GenerateRelay(challengeID, cweID, asset, endpoint string) error {
	if cweID == "" || asset == "" {
		return nil
	}
	form := m.cellForm(challengeID, asset, endpoint)
	if form == "" {
		form = ClassifyForm(endpoint)
	}
	if form == FormStatic || form == FormUnknown {
		return nil // 静态/未知形态:不生成注入类接力(形态未知先 probe)
	}
	cands := CandidateCWEs(form)
	if len(cands) == 0 {
		return nil
	}
	// 已 confirmed 的 CWE 不重复生成(幂等)
	confirmed := m.confirmedCWEs(challengeID, asset, endpoint)
	for _, c := range cands {
		if c == cweID || confirmed[c] {
			continue
		}
		if err := m.EnsureOpen(challengeID, c, asset, endpoint, form); err != nil {
			return err
		}
	}
	// 同 CWE 其他端点:矩阵内同 asset 同形态的其他端点
	return m.relaySameCWEOtherEndpoints(challengeID, cweID, asset, endpoint, form)
}

// cellForm 读取端点已记录的形态(空 = 无记录)。
func (m *Matrix) cellForm(challengeID, asset, endpoint string) string {
	var form sql.NullString
	err := m.db.QueryRow(`SELECT form FROM coverage_matrix
		WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND form IS NOT NULL
		LIMIT 1`, challengeID, asset, endpoint).Scan(&form)
	if err != nil || !form.Valid {
		return ""
	}
	return form.String
}

// confirmedCWEs 返回该端点已确认的 CWE 集合。
func (m *Matrix) confirmedCWEs(challengeID, asset, endpoint string) map[string]bool {
	out := map[string]bool{}
	rows, err := m.db.Query(`SELECT cwe FROM coverage_matrix
		WHERE challenge_id=? AND asset=? AND COALESCE(endpoint,'')=COALESCE(?,'') AND status='confirmed' AND cwe IS NOT NULL`,
		challengeID, asset, endpoint)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cwe sql.NullString
		if rows.Scan(&cwe) == nil && cwe.Valid {
			out[cwe.String] = true
		}
	}
	return out
}

// relaySameCWEOtherEndpoints 同 CWE 展开到同形态的其他端点(03 §3.4)。
func (m *Matrix) relaySameCWEOtherEndpoints(challengeID, cweID, asset, endpoint, form string) error {
	cells, err := m.Cells(challengeID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range cells {
		key := c.Asset + "|" + c.Endpoint
		if seen[key] {
			continue
		}
		seen[key] = true
		if c.Form == form && (c.Asset != asset || c.Endpoint != endpoint) {
			if err := m.EnsureOpen(challengeID, cweID, c.Asset, c.Endpoint, form); err != nil {
				return err
			}
		}
	}
	return nil
}

// ------------------------------------------------------------------ helpers

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
