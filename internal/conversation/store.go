package conversation

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/store"
)

// ErrDigestConflict 表示并发写冲突(读后改前,他人已写入)。
var ErrDigestConflict = errors.New("conversation: digest conflict, session modified concurrently")

// Store 是会话的 SQLite 持久化。
type Store struct {
	db *store.DB
}

// NewStore 构建会话存储。
func NewStore(db *store.DB) *Store { return &Store{db: db} }

// SessionRecord 是数据库行映射。
type SessionRecord struct {
	ID             string
	Kind           SessionKind
	ChallengeID    string
	Provider       string
	Model          string
	Messages       []provider.Message
	RewriteVersion int
	Digest         string
	Branch         Branch
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SaveSnapshot 保存每轮结束快照。对从库中 Load 出来的会话自动做 digest
// 乐观锁:若读取后被他人并发写入,返回 ErrDigestConflict。
func (st *Store) SaveSnapshot(s *Session) error {
	return st.save(s, BranchSnapshot, s.baseDigest)
}

// SaveRewrite 保存压缩后的会话(rewrite 分支)。
func (st *Store) SaveRewrite(s *Session) error {
	return st.save(s, BranchRewrite, "")
}

// SaveRecoveryBranch 保存崩溃恢复分支。恢复分支保证压缩后若发现摘要劣化
// 可回退(三分支保存语义, docs/02 §2.3)。
func (st *Store) SaveRecoveryBranch(s *Session, cause string) error {
	if s.Metadata == nil {
		s.Metadata = map[string]any{}
	}
	s.Metadata["recovery_cause"] = cause
	return st.save(s, BranchRecovery, "")
}

func (st *Store) save(s *Session, branch Branch, expectedDigest string) error {
	digest, err := s.Digest()
	if err != nil {
		return err
	}
	msgsJSON, err := json.Marshal(s.Messages)
	if err != nil {
		return fmt.Errorf("conversation: marshal messages: %w", err)
	}
	metaJSON, err := json.Marshal(s.Metadata)
	if err != nil {
		return fmt.Errorf("conversation: marshal metadata: %w", err)
	}
	now := time.Now().Unix()

	if expectedDigest != "" {
		// 乐观锁:仅当 digest 未变时更新
		res, err := st.db.Exec(`UPDATE sessions SET messages=?, rewrite_version=?, digest=?, metadata=?, updated_at=? 
			WHERE id=? AND digest=?`,
			msgsJSON, s.RewriteVersion, digest, metaJSON, now, s.ID, expectedDigest)
		if err != nil {
			return fmt.Errorf("conversation: save snapshot: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrDigestConflict
		}
		s.baseDigest = digest
		return nil
	}

	_, err = st.db.Exec(`INSERT INTO sessions (id, kind, challenge_id, provider, model, messages, rewrite_version, digest, branch, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, branch) DO UPDATE SET
			messages=excluded.messages, rewrite_version=excluded.rewrite_version,
			digest=excluded.digest, metadata=excluded.metadata,
			updated_at=excluded.updated_at`,
		s.ID, string(s.Kind), s.ChallengeID, s.Provider, s.Model, msgsJSON,
		s.RewriteVersion, digest, string(branch), metaJSON,
		s.CreatedAt.Unix(), now)
	if err != nil {
		return fmt.Errorf("conversation: save: %w", err)
	}
	return nil
}

// Load 按 id 加载会话。
// Load 加载会话最新分支(与 LoadLatest 同语义,保留兼容签名)。
func (st *Store) Load(id string) (*Session, error) {
	return st.LoadLatest(id)
}

// LoadLatest 加载会话最新分支(恢复路径:recovery > rewrite > snapshot)。
func (st *Store) LoadLatest(id string) (*Session, error) {
	// 同一 id 多分支行,按分支优先级 + updated_at 取最新
	order := "CASE branch WHEN 'recovery' THEN 0 WHEN 'rewrite' THEN 1 WHEN 'snapshot' THEN 2 ELSE 3 END, updated_at DESC"
	var branch string
	err := st.db.QueryRow(`SELECT branch FROM sessions WHERE id=? ORDER BY `+order+` LIMIT 1`, id).Scan(&branch)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("conversation: session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	// Load 按主键取;多分支行需按 branch 过滤
	var (
		msgsJSON   []byte
		kind, providerName, model, metaJSON, digest string
		rewriteVersion int
		createdAt, updatedAt int64
	)
	err = st.db.QueryRow(`SELECT kind, provider, model, messages, rewrite_version, digest, metadata, created_at, updated_at
		FROM sessions WHERE id=? AND branch=?`, id, branch).
		Scan(&kind, &providerName, &model, &msgsJSON, &rewriteVersion, &digest, &metaJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("conversation: load latest %s: %w", id, err)
	}
	var msgs []provider.Message
	if err := json.Unmarshal(msgsJSON, &msgs); err != nil {
		return nil, err
	}
	var meta map[string]any
	if metaJSON != "" {
		_ = json.Unmarshal([]byte(metaJSON), &meta)
	}
	return &Session{
		ID: id, Kind: SessionKind(kind), Provider: providerName, Model: model,
		Messages: msgs, RewriteVersion: rewriteVersion, Metadata: meta,
		CreatedAt: time.Unix(createdAt, 0), UpdatedAt: time.Unix(updatedAt, 0),
		baseDigest: digest,
	}, nil
}

// LoadRecoveryBranch 加载会话的 recovery 分支(损坏回滚路径,无则 nil)。
func (st *Store) LoadRecoveryBranch(id string) (*Session, error) {
	var (
		msgsJSON   []byte
		kind, providerName, model, metaJSON, digest string
		rewriteVersion int
		createdAt, updatedAt int64
	)
	err := st.db.QueryRow(`SELECT kind, provider, model, messages, rewrite_version, digest, metadata, created_at, updated_at
		FROM sessions WHERE id=? AND branch='recovery' ORDER BY updated_at DESC LIMIT 1`, id).
		Scan(&kind, &providerName, &model, &msgsJSON, &rewriteVersion, &digest, &metaJSON, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("conversation: load recovery %s: %w", id, err)
	}
	var msgs []provider.Message
	if err := json.Unmarshal(msgsJSON, &msgs); err != nil {
		return nil, err
	}
	var meta map[string]any
	if metaJSON != "" {
		_ = json.Unmarshal([]byte(metaJSON), &meta)
	}
	return &Session{
		ID: id, Kind: SessionKind(kind), Provider: providerName, Model: model,
		Messages: msgs, RewriteVersion: rewriteVersion, Metadata: meta,
		CreatedAt: time.Unix(createdAt, 0), UpdatedAt: time.Unix(updatedAt, 0),
		baseDigest: digest,
	}, nil
}

// ValidatePairing 校验消息序列配对完整性(docs/03 §7 会话损坏检测):
//   - assistant 声明的每个 tool_call 必须有对应 tool 结果(或 LocalOnly 标记)
//   - 游离的 tool 消息(无前驱 assistant)视为损坏
//
// 返回损坏原因,空串 = 健康。
func ValidatePairing(msgs []provider.Message) string {
	openCalls := 0
	for _, m := range msgs {
		if m.LocalOnly {
			continue
		}
		switch m.Role {
		case provider.RoleAssistant:
			openCalls += len(m.ToolCalls)
		case provider.RoleTool:
			if openCalls <= 0 {
				return "orphan tool message without preceding assistant tool_call"
			}
			openCalls--
		}
	}
	if openCalls > 0 {
		return fmt.Sprintf("%d tool_call(s) without matching tool result", openCalls)
	}
	return ""
}

// List 按 kind 列出会话元数据(不含消息)。
func (st *Store) List(kind string) ([]SessionRecord, error) {
	rows, err := st.db.Query(`SELECT id, kind, challenge_id, provider, model, rewrite_version, digest, branch, created_at, updated_at
		FROM sessions WHERE (? = '' OR kind = ?) ORDER BY updated_at DESC`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRecord
	for rows.Next() {
		var r SessionRecord
		var createdAt, updatedAt int64
		if err := rows.Scan(&r.ID, &r.Kind, &r.ChallengeID, &r.Provider, &r.Model,
			&r.RewriteVersion, &r.Digest, &r.Branch, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		r.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete 删除会话及其全部分支。
func (st *Store) Delete(id string) error {
	_, err := st.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
	return err
}
