// Package breach 是 goal_shape=breach 的可达状态空间(docs/phase2/03 §3.4.1)。
//
// 状态图:节点 = 已确认状态(主机/权限/凭据),边 = 转移动作。
// 发现/达成一个状态后,确定性生成转移候选(三层机制第 1 层):
//
//	有 shell     → {横向移动, 提权, 凭据收集}
//	有域用户     → {域枚举, 委派攻击, 密码喷洒(限速)}
//	有 web 入口  → {注入→RCE, 上传→RCE, SSRF→内网}
//	有凭据       → {登录, 凭据复用}
//
// 能力维度(privilege,2.22 起):节点除 kind(资产/入口形态)外携带 privilege
// (权限能力集合,逗号分隔)。能力与攻击路径解耦——JWT 伪造/万能密码/SSRF
// 绕过登录等任意路径都汇聚为同一能力 authenticated,由 auth_gained 声明
// 确定性落地;候选动作 = kind 基线 ∪ privilege 解锁动作(路径无限,能力有界)。
//
//	已认证(authenticated) → +{登录后数据访问, 管理功能利用, 越权深挖, 凭据收集}
//
// 候选只进状态图(open 边),调度按"距 goal 启发式"认领(03 §3.2 breach 分支);
// agent 补充:explore 收尾可回报新转移方向(new_intent 同机制)。
//
// 终止(02 §2.2 ②'②”):
//   - goal_reached:goal 断言被独立验证器确认(节点 confirmed),模型自述不采信
//   - space_closed:所有开放边均终态(confirmed|dead|skipped),无运行中 worker
package breach

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"scopeforge/internal/store"
)

// 状态种类(03 §3.4.1 权限形态 × 可达目标映射的输入)。
const (
	KindWeb        = "web"         // web 入口
	KindShell      = "shell"       // 主机 shell
	KindDomainUser = "domain_user" // 域用户
	KindHost       = "host"        // 主机(已确认可达)
	KindCredential = "credential"  // 凭据
)

// 转移动作(03 §3.4.1 候选动作)。
const (
	ActionLateral     = "横向移动"
	ActionPrivEsc     = "提权"
	ActionCredGrab    = "凭据收集"
	ActionDomainEnum  = "域枚举"
	ActionDelegate    = "委派攻击"
	ActionSpray       = "密码喷洒"
	ActionInjectRCE   = "注入→RCE"
	ActionUploadRCE   = "上传→RCE"
	ActionSSRFInner   = "SSRF→内网"
	ActionLogin       = "登录"      // 用凭据登录目标服务(凭据利用)
	ActionCredReuse   = "凭据复用"    // 同凭据尝试其他资产(横向)
	ActionAuthData    = "登录后数据访问" // 访问登录后才可见的数据/接口
	ActionAuthAdmin   = "管理功能利用"  // 后台/管理功能
	ActionAuthPrivEsc = "越权深挖"    // 登录后水平/垂直越权
)

// 权限能力(privilege 维度,逗号分隔能力集合;2.22 起由 auth_gained 声明落地,
// 2.23 起契约 privilege 字段通用声明)。
const (
	PrivilegeAuthenticated = "authenticated" // 已认证/登录态
	PrivilegeShell         = "shell"         // 主机 shell
	PrivilegeDomainUser    = "domain_user"   // 域用户
)

// 边状态。
const (
	EdgeOpen      = "open"
	EdgeClaimed   = "claimed"
	EdgeConfirmed = "confirmed"
	EdgeDead      = "dead"
	EdgeSkipped   = "skipped"
)

// Node 是状态图节点。
type Node struct {
	ID        string `json:"id"`
	Challenge string `json:"challenge_id"`
	Kind      string `json:"kind"`
	Asset     string `json:"asset"`
	Privilege string `json:"privilege"`
	Confirmed bool   `json:"confirmed"`
}

// Edge 是状态转移边。
type Edge struct {
	ID         string `json:"id"`
	Challenge  string `json:"challenge_id"`
	From       string `json:"from_node"`
	Action     string `json:"action"`
	To         string `json:"to_node"`
	Status     string `json:"status"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// Space 是 breach 状态空间(持久化 breach_nodes/breach_edges)。
type Space struct {
	db       *store.DB
	goalNode string // 配置的目标状态节点(独立验证器判定目标)
}

// New 构建状态空间。
func New(db *store.DB) *Space { return &Space{db: db} }

// ------------------------------------------------------------------ 节点

// ConfirmNode 确认一个状态(独立验证器确认后才调用,模型自述不采信)。
// privilege 为能力集合追加项:已确认节点再次确认时能力累加合并
// (如 "shell" + "authenticated" → "shell,authenticated",不覆盖不重复)。
// 返回是否新建(已确认则幂等)。
func (s *Space) ConfirmNode(challengeID, id, kind, asset, privilege string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO breach_nodes (id, challenge_id, kind, asset, privilege, confirmed, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET
			confirmed=1,
			kind=COALESCE(NULLIF(?,''), kind),
			asset=COALESCE(NULLIF(?,''), asset),
			privilege=CASE
				WHEN privilege IS NULL OR privilege='' THEN ?
				WHEN instr(','||privilege||',', ','||?||',')>0 THEN privilege
				ELSE privilege||','||? END`,
		id, challengeID, kind, asset, nullIfEmpty(privilege), now,
		kind, asset, nullIfEmpty(privilege), privilege, privilege)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NodeByID 读取节点。
func (s *Space) NodeByID(id string) (*Node, error) {
	var n Node
	var priv sql.NullString
	err := s.db.QueryRow(`SELECT id, challenge_id, kind, asset, privilege, confirmed FROM breach_nodes WHERE id=?`, id).
		Scan(&n.ID, &n.Challenge, &n.Kind, &n.Asset, &priv, &n.Confirmed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.Privilege = priv.String
	return &n, nil
}

// ConfirmedNodes 读取已确认节点(goals 判定数据源)。
func (s *Space) ConfirmedNodes(challengeID string) ([]Node, error) {
	rows, err := s.db.Query(`SELECT id, challenge_id, kind, asset, privilege, confirmed
		FROM breach_nodes WHERE challenge_id=? AND confirmed=1`, challengeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var priv sql.NullString
		if err := rows.Scan(&n.ID, &n.Challenge, &n.Kind, &n.Asset, &priv, &n.Confirmed); err != nil {
			return nil, err
		}
		n.Privilege = priv.String
		out = append(out, n)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ 边

// AddTransitions 为已确认节点生成转移候选边(03 §3.4.1 第 1 层,幂等)。
// 候选 = kind 基线 ∪ privilege 解锁动作;已有同 (from, action) 边则不动。
// 节点能力升级(如 auth_gained → authenticated)后重复调用可补齐新解锁边。
func (s *Space) AddTransitions(challengeID, fromNode string) error {
	n, err := s.NodeByID(fromNode)
	if err != nil || n == nil {
		return err
	}
	if !n.Confirmed {
		return nil // 源状态未确认不生成(状态空间由发现动态展开)
	}
	now := time.Now().Unix()
	for _, action := range candidatesFor(n.Kind, n.Privilege) {
		edgeID := fromNode + "|" + action
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO breach_edges
			(id, challenge_id, from_node, action, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'open', ?, ?)`,
			edgeID, challengeID, fromNode, action, now, now); err != nil {
			return err
		}
	}
	return nil
}

// AddAgentEdge 将 explore 回报的新转移方向(new_intent)落地为自由方向边
// (03 §3.4.1 第 3 层 agent 补充;2.22 接通 breach 认领池)。锚定在来源节点
// (执行中边的 from)上:来源节点不存在则不建(防脏数据)。幂等:同
// (from, action) 已存在(含确定性候选)则忽略。
func (s *Space) AddAgentEdge(challengeID, fromNode, action string) error {
	if action == "" {
		return nil
	}
	n, err := s.NodeByID(fromNode)
	if err != nil || n == nil {
		return err
	}
	now := time.Now().Unix()
	edgeID := fromNode + "|" + action
	_, err = s.db.Exec(`INSERT OR IGNORE INTO breach_edges
		(id, challenge_id, from_node, action, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'open', ?, ?)`,
		edgeID, challengeID, fromNode, action, now, now)
	return err
}

// candidatesFor 权限形态(kind)× 能力(privilege) → 受限转移候选
// (非全组合,03 §3.4.1)。kind 提供基线动作(旧数据兼容,存量节点 kind 含
// shell/domain_user),privilege 解锁能力动作(2.23 通用声明通道,优先);
// 攻击路径不参与映射(路径无限,能力有界)。
func candidatesFor(kind, privilege string) []string {
	var out []string
	switch kind {
	case KindShell:
		out = []string{ActionLateral, ActionPrivEsc, ActionCredGrab}
	case KindDomainUser:
		out = []string{ActionDomainEnum, ActionDelegate, ActionSpray}
	case KindWeb:
		out = []string{ActionInjectRCE, ActionUploadRCE, ActionSSRFInner}
	case KindCredential:
		// 拿到凭据(含 auth_gained 声明)后:① 用凭据登录目标服务展开登录后
		// 攻击面;② 同凭据尝试其他资产(凭据复用/横向,如默认口令批量尝试)。
		out = []string{ActionLogin, ActionCredReuse}
	}
	// privilege 能力解锁(dedupe 合并 kind 基线,重复不产生新边)
	if hasPrivilege(privilege, PrivilegeShell) {
		// 有 shell → 横向移动/提权/凭据收集
		out = append(out, ActionLateral, ActionPrivEsc, ActionCredGrab)
	}
	if hasPrivilege(privilege, PrivilegeDomainUser) {
		// 有域用户 → 域枚举/委派攻击/密码喷洒(限速)
		out = append(out, ActionDomainEnum, ActionDelegate, ActionSpray)
	}
	if hasPrivilege(privilege, PrivilegeAuthenticated) {
		// 已认证(登录态):登录后攻击面动作(2.22,B2 能力维度)
		out = append(out, ActionAuthData, ActionAuthAdmin, ActionAuthPrivEsc, ActionCredGrab)
	}
	return dedupe(out)
}

// hasPrivilege 判定能力集合(逗号分隔)是否含目标能力。
func hasPrivilege(set, want string) bool {
	if set == "" || want == "" {
		return false
	}
	for _, p := range strings.Split(set, ",") {
		if strings.TrimSpace(p) == want {
			return true
		}
	}
	return false
}

// dedupe 保序去重。
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ClaimEdge 认领边(调度派活时)。
func (s *Space) ClaimEdge(edgeID string) error {
	_, err := s.db.Exec(`UPDATE breach_edges SET status='claimed', updated_at=? WHERE id=? AND status='open'`,
		time.Now().Unix(), edgeID)
	return err
}

// UnclaimEdge 回滚认领(派活硬失败时,防状态空间卡死)。
func (s *Space) UnclaimEdge(edgeID string) error {
	_, err := s.db.Exec(`UPDATE breach_edges SET status='open', updated_at=? WHERE id=? AND status='claimed'`,
		time.Now().Unix(), edgeID)
	return err
}

// ConfirmEdge 边达成(explore 回报达成目标状态时)。
func (s *Space) ConfirmEdge(challengeID, edgeID, toNode string) error {
	_, err := s.db.Exec(`UPDATE breach_edges SET status='confirmed', to_node=?, updated_at=?
		WHERE id=? AND challenge_id=?`,
		nullIfEmpty(toNode), time.Now().Unix(), edgeID, challengeID)
	return err
}

// MarkEdgeDead 边失败(方向耗尽)。
func (s *Space) MarkEdgeDead(edgeID string) error {
	_, err := s.db.Exec(`UPDATE breach_edges SET status='dead', updated_at=? WHERE id=? AND status IN ('open','claimed')`,
		time.Now().Unix(), edgeID)
	return err
}

// Edges 读取全部边。
func (s *Space) Edges(challengeID string) ([]Edge, error) {
	rows, err := s.db.Query(`SELECT id, challenge_id, from_node, action, to_node, status, skip_reason
		FROM breach_edges WHERE challenge_id=? ORDER BY created_at ASC`, challengeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		var to, skip sql.NullString
		if err := rows.Scan(&e.ID, &e.Challenge, &e.From, &e.Action, &to, &e.Status, &skip); err != nil {
			return nil, err
		}
		e.To = to.String
		e.SkipReason = skip.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// OpenEdges 读取 open 边(调度认领数据源,按距 goal 启发式由调用方排序)。
func (s *Space) OpenEdges(challengeID string) ([]Edge, error) {
	edges, err := s.Edges(challengeID)
	if err != nil {
		return nil, err
	}
	var out []Edge
	for _, e := range edges {
		if e.Status == EdgeOpen {
			out = append(out, e)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ 终止判定(constraint.GoalVerifier)

// IsGoalReached 实现 constraint.GoalVerifier(02 §2.3):
// goal 由调用方配置(goalNodeID);已确认节点命中 goal → true。
// 模型自述不采信——节点 confirmed 只来自独立验证器(ConfirmNode 调用方)。
func (s *Space) IsGoalReached(ctx context.Context, taskID string) (bool, string) {
	if s.goalNode == "" {
		return false, ""
	}
	n, err := s.NodeByID(s.goalNode)
	if err != nil || n == nil {
		return false, ""
	}
	if n.Confirmed {
		return true, s.goalNode
	}
	return false, ""
}

// IsSpaceClosed 实现 constraint.GoalVerifier(02 §2.3):
// 所有边终态(confirmed|dead|skipped)且无 open/claimed 边 → space_closed。
func (s *Space) IsSpaceClosed(ctx context.Context, taskID string) (bool, string) {
	edges, err := s.Edges(taskID)
	if err != nil {
		return false, "breach_error"
	}
	if len(edges) == 0 {
		return false, "no_state_space" // 状态空间都没展开,不算闭合
	}
	for _, e := range edges {
		if e.Status == EdgeOpen || e.Status == EdgeClaimed {
			return false, "open_edge: " + e.ID
		}
	}
	return true, "space_closed"
}

// SetGoal 注入 goal 节点(独立验证器的判定目标;空 = 不启用 goal_reached 路)。
func (s *Space) SetGoal(nodeID string) { s.goalNode = nodeID }

// maxDist 是图不可达距离(03 §3.2 距 goal 启发式:不可达边排最后)。
const maxDist = 1 << 20

// DistToGoal 计算 from 节点到 goal 节点的无权图最短距离(边数,BFS)。
// 图 = 全部状态转移边(open/claimed/confirmed/dead 均计,方向可达性);
// goal 未配置或不可达 → maxDist。
func (s *Space) DistToGoal(challengeID, from string) int {
	if s.goalNode == "" {
		return maxDist
	}
	if from == s.goalNode {
		return 0
	}
	edges, err := s.Edges(challengeID)
	if err != nil {
		return maxDist
	}
	adj := map[string][]string{}
	for _, e := range edges {
		if e.To != "" {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}
	dist := map[string]int{from: 0}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == s.goalNode {
			return dist[cur]
		}
		for _, nxt := range adj[cur] {
			if _, seen := dist[nxt]; !seen {
				dist[nxt] = dist[cur] + 1
				queue = append(queue, nxt)
			}
		}
	}
	return maxDist
}

// ------------------------------------------------------------------ helpers

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EdgeByFromAction 查询 (from, action) 边(agent 回报转移方向时复用)。
func (s *Space) EdgeByFromAction(challengeID, fromNode, action string) (*Edge, error) {
	var e Edge
	var to, skip sql.NullString
	err := s.db.QueryRow(`SELECT id, challenge_id, from_node, action, to_node, status, skip_reason
		FROM breach_edges WHERE challenge_id=? AND from_node=? AND action=?`, challengeID, fromNode, action).
		Scan(&e.ID, &e.Challenge, &e.From, &e.Action, &to, &e.Status, &skip)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.To = to.String
	e.SkipReason = skip.String
	return &e, nil
}

// EdgeByID 按 ID 查询边(2.22:agent 自由边锚点解析用)。
func (s *Space) EdgeByID(challengeID, edgeID string) (*Edge, error) {
	var e Edge
	var to, skip sql.NullString
	err := s.db.QueryRow(`SELECT id, challenge_id, from_node, action, to_node, status, skip_reason
		FROM breach_edges WHERE challenge_id=? AND id=?`, challengeID, edgeID).
		Scan(&e.ID, &e.Challenge, &e.From, &e.Action, &to, &e.Status, &skip)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.To = to.String
	e.SkipReason = skip.String
	return &e, nil
}
