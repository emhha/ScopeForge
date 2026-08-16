// Package planner 是计划模式与子代理派生(docs/02 §7.3)。
// task/read_only_task 工具派生独立 Agent 循环:隔离 registry、深度限制、
// 并发上限、转录存 subagent_transcripts 表。
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"scopeforge/internal/conversation"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/store"
)

// 上下文键:子代理深度。
type depthKey struct{}

// WithDepth 在 ctx 中携带子代理深度。
func WithDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, depthKey{}, depth)
}

// DepthOf 读取当前深度(根代理为 0)。
func DepthOf(ctx context.Context) int {
	if d, ok := ctx.Value(depthKey{}).(int); ok {
		return d
	}
	return 0
}

// 递归工具:子代理中剔除,防无限递归(docs/02 §7.3)。
var recursiveTools = map[string]bool{
	"task": true, "read_only_task": true, "fleet": true,
	"run_skill": true, "read_only_skill": true,
}

// TranscriptStore 是子代理转录存储(subagent_transcripts 表)。
type TranscriptStore struct {
	db *store.DB
}

// NewTranscriptStore 构建转录存储。
func NewTranscriptStore(db *store.DB) *TranscriptStore { return &TranscriptStore{db: db} }

// Save 保存一条转录,返回 id。
func (s *TranscriptStore) Save(parentID string, depth int, prompt, model string) (string, error) {
	id := fmt.Sprintf("sa_%d", time.Now().UnixNano())
	_, err := s.db.Exec(`INSERT INTO subagent_transcripts (id, parent_id, depth, prompt, status, model, created_at)
		VALUES (?, ?, ?, ?, 'running', ?, ?)`,
		id, parentID, depth, prompt, model, time.Now().Unix())
	if err != nil {
		return "", fmt.Errorf("planner: save transcript: %w", err)
	}
	return id, nil
}

// Finish 结束转录。
func (s *TranscriptStore) Finish(id, result, status string) error {
	_, err := s.db.Exec(`UPDATE subagent_transcripts SET result=?, status=?, completed_at=? WHERE id=?`,
		result, status, time.Now().Unix(), id)
	return err
}

// TaskTool 是子代理派发工具。
type TaskTool struct {
	Provider     provider.Provider
	Registry     *tool.Registry // 父注册表(过滤递归工具后给子代理)
	ConvStore    *conversation.Store
	TransStore   *TranscriptStore
	Gate         *executor.Gate
	Sink         event.Sink
	WorkDir      string
	SystemPrompt string
	CompactCfg   conversation.CompactConfig
	MaxDepth     int // 默认 2
	// Semaphore 并发上限(默认 6)。
	Semaphore chan struct{}
}

// Name/Description/Schema/ReadOnly 实现 tool.Tool 契约。
func (t *TaskTool) Name() string        { return "task" }
func (t *TaskTool) Description() string { return "派生一个独立子代理完成任务,返回其最终回答。" }
func (t *TaskTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"prompt":{"type":"string","description":"子代理的任务描述"},
		"description":{"type":"string"},
		"read_only":{"type":"boolean"},
		"write_paths":{"type":"array","items":{"type":"string"}},
		"max_steps":{"type":"number"},
		"model":{"type":"string"}
	},"required":["prompt"]}`)
}
func (t *TaskTool) ReadOnly() bool { return true }

// Execute 派生子代理。
func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Prompt      string   `json:"prompt"`
		Description string   `json:"description"`
		ReadOnly    bool     `json:"read_only"`
		WritePaths  []string `json:"write_paths"`
		MaxSteps    int      `json:"max_steps"`
		Model       string   `json:"model"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("task: bad args: %v", err)
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", fmt.Errorf("task: prompt required")
	}

	// 深度限制(docs/02 §7.3:max_subagent_depth 防无限递归)
	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	depth := DepthOf(ctx)
	if depth+1 > maxDepth {
		return "", fmt.Errorf("subagent delegation depth limit reached (max_subagent_depth=%d)", maxDepth)
	}

	// 并发上限(默认 6)
	sem := t.Semaphore
	if sem == nil {
		sem = make(chan struct{}, 6)
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// 转录
	transID, err := t.TransStore.Save("", depth, p.Prompt, p.Model)
	if err != nil {
		return "", err
	}

	// 子代理会话
	child := conversation.New(transID, conversation.KindSubagent)
	child.Add(provider.Message{Role: provider.RoleUser, Content: p.Prompt})

	// 隔离 registry:剔除递归工具;read_only 时只留只读工具
	subReg := tool.NewRegistry()
	if t.Registry != nil {
		for _, name := range t.Registry.Names() {
			if recursiveTools[name] {
				continue
			}
			if p.ReadOnly {
				if tl, ok := t.Registry.Get(name); ok && !tl.ReadOnly() {
					continue
				}
			}
			if tl, ok := t.Registry.Get(name); ok {
				subReg.Add(tl)
			}
		}
	}

	childMaxSteps := p.MaxSteps
	if childMaxSteps <= 0 {
		childMaxSteps = 50
	}
	opts := &executor.Options{
		Provider:     t.Provider,
		Registry:     subReg,
		Session:      child,
		Store:        t.ConvStore,
		MaxTurns:     childMaxSteps,
		Gate:         t.Gate,
		Sink:         t.Sink,
		CompactCfg:   t.CompactCfg,
		SystemPrompt: t.SystemPrompt,
		WorkDir:      t.WorkDir,
	}
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	if opts.Gate == nil {
		opts.Gate, _ = executor.NewGate(executor.ModeYolo, nil, nil)
	}

	res, err := executor.Run(WithDepth(ctx, depth+1), opts)

	status := "completed"
	if err != nil {
		status = "failed"
	}
	_ = t.TransStore.Finish(transID, res.FinalText, status)

	if err != nil {
		return fmt.Sprintf("[subagent failed: %v]\n%s", err, res.FinalText), err
	}
	return fmt.Sprintf("[subagent %s 完成]\n%s\n(转录引用: %s)", transID, res.FinalText, transID), nil
}
