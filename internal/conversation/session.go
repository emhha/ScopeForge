// Package conversation 是会话模型与上下文管理:消息序列的增删改写、持久化、
// 上下文窗口管理与压缩。压缩是上下文腐败的第一防线,触发时机必须宿主侧硬控。
//
// 设计契约: docs/02 §2。持久化由 JSON 文件改造为 SQLite 表(docs/01 §4)。
package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"scopeforge/internal/reasonix/provider"
)

// SessionKind 是会话种类。
type SessionKind string

const (
	KindMain     SessionKind = "main"     // 用户主会话
	KindWorker   SessionKind = "worker"   // M1 Worker 池会话
	KindSubagent SessionKind = "subagent" // 子代理会话
	KindObserver SessionKind = "observer" // M1 Observer 会话
)

// Branch 是保存分支标记。
type Branch string

const (
	BranchMain     Branch = "main"     // 常规快照
	BranchSnapshot Branch = "snapshot" // 每轮结束快照
	BranchRewrite  Branch = "rewrite"  // 压缩后重写
	BranchRecovery Branch = "recovery" // 崩溃恢复分支
)

// Session 是会话模型。
type Session struct {
	ID             string
	Kind           SessionKind
	ChallengeID    string
	Provider       string
	Model          string
	Messages       []provider.Message
	RewriteVersion int
	Metadata       map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time

	baseDigest string // 读取时的 digest(乐观锁基础),由 Load 填充
}

// New 创建新会话。
func New(id string, kind SessionKind) *Session {
	now := time.Now()
	return &Session{
		ID:        id,
		Kind:      kind,
		Metadata:  map[string]any{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Add 追加消息。
func (s *Session) Add(msg provider.Message) {
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = time.Now()
}

// Replace 替换第 i 条消息。
func (s *Session) Replace(i int, msg provider.Message) error {
	if i < 0 || i >= len(s.Messages) {
		return fmt.Errorf("conversation: replace index %d out of range (len %d)", i, len(s.Messages))
	}
	s.Messages[i] = msg
	s.UpdatedAt = time.Now()
	return nil
}

// Rewrite 整体重写消息序列(压缩后),rewrite_version +1。
func (s *Session) Rewrite(msgs []provider.Message) {
	s.Messages = msgs
	s.RewriteVersion++
	s.UpdatedAt = time.Now()
}

// Snapshot 返回消息序列副本(防外部修改)。
func (s *Session) Snapshot() []provider.Message {
	out := make([]provider.Message, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// CloneWithMessages 以给定消息序列克隆会话(元数据与版本继承)。
func (s *Session) CloneWithMessages(msgs []provider.Message) *Session {
	clone := &Session{
		ID:             s.ID,
		Kind:           s.Kind,
		ChallengeID:    s.ChallengeID,
		Provider:       s.Provider,
		Model:          s.Model,
		Messages:       msgs,
		RewriteVersion: s.RewriteVersion,
		Metadata:       s.Metadata,
		CreatedAt:      s.CreatedAt,
		UpdatedAt:      time.Now(),
	}
	return clone
}

// Digest 计算消息序列的 sha256 摘要,防并发写冲突。
func (s *Session) Digest() (string, error) {
	data, err := json.Marshal(s.Messages)
	if err != nil {
		return "", fmt.Errorf("conversation: digest marshal: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
