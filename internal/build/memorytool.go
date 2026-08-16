package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"scopeforge/internal/reasonix/memory"
)

// memoryTool 是记忆工具(remember/forget/recall, docs/02 §6 最小形态)。
type memoryTool struct {
	store memory.Store
}

func (m *memoryTool) Name() string        { return "remember" }
func (m *memoryTool) Description() string { return "保存一条长期记忆事实。" }
func (m *memoryTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"fact":{"type":"string","description":"记忆内容"},
		"name":{"type":"string","description":"记忆名(kebab-case),缺省自动生成"},
		"scope":{"type":"string","enum":["project","global"]}
	},"required":["fact"]}`)
}
func (m *memoryTool) ReadOnly() bool { return false }

func (m *memoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Fact  string `json:"fact"`
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("remember: %v", err)
	}
	name := p.Name
	if name == "" {
		name = slugify(p.Fact)
	}
	mem := memory.Memory{
		Name:        name,
		Title:       name,
		Description: truncate(p.Fact, 120),
		Type:        memory.TypeProject,
		Scope:       memory.FactScopeProject,
		Body:        p.Fact,
	}
	if p.Scope == "global" {
		mem.Scope = memory.FactScopeGlobal
	}
	path, err := m.store.Save(mem)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("remembered %s (%s)", name, path), nil
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fmt.Sprintf("fact-%d", len(s))
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
