// Package toolkit implements the platform's business tools using native tRPC declarations.
package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type Output struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func Text(s string) Output  { return Output{Text: s} }
func Error(s string) Output { return Output{Text: s, IsError: true} }
func TextOf(v any) string {
	switch x := v.(type) {
	case Output:
		return x.Text
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
func Failed(v any) bool {
	switch x := v.(type) {
	case Output:
		return x.IsError
	case map[string]any:
		if failed, ok := x["is_error"].(bool); ok && failed {
			return true
		}
		if x["error"] != nil {
			return true
		}
		switch code := x["exit_code"].(type) {
		case int:
			return code != 0
		case float64:
			return code != 0
		case int64:
			return code != 0
		}
	}
	return false
}
func FailedJSON(text string) bool {
	var value map[string]any
	// HTTP output starts with JSON metadata and then a literal response body.
	// Inspect only the first object; response text must never change tool status.
	if json.NewDecoder(strings.NewReader(text)).Decode(&value) == nil {
		return Failed(value)
	}
	return strings.HasPrefix(text, "Error:") || strings.HasPrefix(text, "Error executing")
}
func Schema(m map[string]any) *tool.Schema {
	b, _ := json.Marshal(m)
	var s tool.Schema
	_ = json.Unmarshal(b, &s)
	return &s
}
func SchemaMap(s *tool.Schema) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

type businessTool struct {
	declaration *tool.Declaration
	run         func(context.Context, json.RawMessage) (Output, error)
}

func New(name, description string, schema map[string]any, run func(context.Context, json.RawMessage) (Output, error)) tool.CallableTool {
	return &businessTool{&tool.Declaration{Name: name, Description: description, InputSchema: Schema(schema)}, run}
}
func (t *businessTool) Declaration() *tool.Declaration { return t.declaration }
func (t *businessTool) Call(ctx context.Context, args []byte) (result any, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("tool %s: %v", t.declaration.Name, r)
		}
	}()
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	if !json.Valid(args) {
		return nil, fmt.Errorf("invalid JSON arguments for %s", t.declaration.Name)
	}
	return t.run(ctx, args)
}

type Workspace struct {
	Directory       string
	Env             []string
	OutputDirectory string
}
type workspaceKey struct{}

func WithWorkspace(ctx context.Context, w Workspace) context.Context {
	return context.WithValue(ctx, workspaceKey{}, w)
}
func WorkspaceFrom(ctx context.Context) Workspace {
	w, _ := ctx.Value(workspaceKey{}).(Workspace)
	return w
}
func Capture(ctx context.Context, text string) string {
	if len([]rune(text)) <= 30000 {
		return text
	}
	w := WorkspaceFrom(ctx)
	r := []rune(text)
	head := string(r[:30000])
	if w.OutputDirectory == "" {
		return head + "\n（输出已截断）"
	}
	if os.MkdirAll(w.OutputDirectory, 0700) != nil {
		return head + "\n（输出已截断）"
	}
	path := filepath.Join(w.OutputDirectory, uuid.NewString()+".txt")
	if os.WriteFile(path, []byte(text), 0600) != nil {
		return head + "\n（输出已截断）"
	}
	return strings.TrimSpace(head) + "\n完整输出：" + path
}

// Named changes a product-facing tool name without changing the SDK schema or executor.
type named struct {
	tool.CallableTool
	name string
}

func (n *named) Declaration() *tool.Declaration {
	copy := *n.CallableTool.Declaration()
	copy.Name = n.name
	return &copy
}
func Named(t tool.CallableTool, name string) tool.CallableTool { return &named{t, name} }
