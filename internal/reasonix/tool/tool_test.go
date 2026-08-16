package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"scopeforge/internal/reasonix/sandbox"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/reasonix/tool/builtin"
)

// newRegistry 构建绑定工作区的工具注册表。
func newRegistry(t *testing.T, dir string) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	ws := builtin.Workspace{Dir: dir, Bash: sandbox.Spec{Mode: "off"}, BashTimeout: 10 * time.Second}
	for _, tl := range ws.Tools() {
		r.Add(tl)
	}
	return r
}

func TestRegistrySemantics(t *testing.T) {
	r := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir(), Bash: sandbox.Spec{Mode: "off"}}
	for _, tl := range ws.Tools("read_file", "write_file") {
		r.Add(tl)
	}
	if r.Len() != 2 {
		t.Fatalf("len = %d, want 2", r.Len())
	}
	// Get
	if _, ok := r.Get("read_file"); !ok {
		t.Error("read_file missing")
	}
	// ResolveCall
	tl, canonical, candidates := r.ResolveCall("read_file")
	if tl == nil || len(candidates) != 0 || !strings.Contains(canonical, "read_file") {
		t.Errorf("resolve = %v %q %v", tl, canonical, candidates)
	}
	// SuspendPrefix:直接移除该前缀工具
	r.SuspendPrefix("read")
	if _, ok := r.Get("read_file"); ok {
		t.Error("suspended tool should be removed from registry")
	}
	// ResumePrefix:允许未来 Add 恢复(工具不会自动回来)
	r.ResumePrefix("read")
	ws2 := builtin.Workspace{Dir: t.TempDir(), Bash: sandbox.Spec{Mode: "off"}}
	for _, tl := range ws2.Tools("read_file") {
		r.Add(tl)
	}
	if _, ok := r.Get("read_file"); !ok {
		t.Error("resumed prefix should accept re-Add")
	}
	// Schemas 非空
	if len(r.Schemas()) != 2 {
		t.Errorf("schemas = %d", len(r.Schemas()))
	}
	// RemovePrefix
	r.RemovePrefix("read")
	if r.Len() != 1 {
		t.Errorf("len after remove = %d", r.Len())
	}
}

func TestFileToolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)

	// write_file
	out, err := mustGet(r, "write_file").Execute(context.Background(), json.RawMessage(`{"path":"a.txt","content":"hello world"}`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out, "wrote") {
		t.Errorf("write out = %q", out)
	}
	// read_file
	out, err = mustGet(r, "read_file").Execute(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("read out = %q", out)
	}
	// edit_file
	out, err = mustGet(r, "edit_file").Execute(context.Background(), json.RawMessage(`{"path":"a.txt","old_string":"hello","new_string":"goodbye"}`))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(out, "edited") {
		t.Errorf("edit out = %q", out)
	}
	// 写工具受 WriteRoots 约束:工作区外绝对路径被拒
	_, err = mustGet(r, "write_file").Execute(context.Background(), json.RawMessage(`{"path":"/tmp/scopeforge-escape-test.txt","content":"x"}`))
	if err == nil {
		t.Error("write outside workspace should be rejected")
	}
	// grep
	out, err = mustGet(r, "grep").Execute(context.Background(), json.RawMessage(`{"pattern":"goodbye"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "goodbye") {
		t.Errorf("grep out = %q", out)
	}
}

func TestBashTool(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)
	out, err := mustGet(r, "bash").Execute(context.Background(), json.RawMessage(`{"command":"echo hello from bash"}`))
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "hello from bash") {
		t.Errorf("bash out = %q", out)
	}
	// 超时斩杀
	start := time.Now()
	_, err = mustGet(r, "bash").Execute(context.Background(), json.RawMessage(`{"command":"sleep 30","timeout":1}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 15*time.Second {
		t.Error("timeout took too long")
	}
}

func TestWebFetch(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)
	// 本地 HTTP 服务器(通过 sandbox 禁网?不:直接测试 SSRF 防护在工具内)
	_, err := mustGet(r, "web_fetch").Execute(context.Background(), json.RawMessage(`{"url":"http://169.254.169.254/latest/meta-data"}`))
	if err == nil {
		t.Error("cloud metadata should be blocked")
	}
	// 非法 scheme
	_, err = mustGet(r, "web_fetch").Execute(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`))
	if err == nil {
		t.Error("file scheme should be blocked")
	}
}

func TestBuiltinToolList(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)
	names := strings.Join(r.Names(), ",")
	for _, want := range []string{"bash", "read_file", "write_file", "edit_file", "grep", "glob", "ls", "web_fetch", "notebook_edit", "code_index", "todo_write", "complete_step", "move_file", "delete_range", "delete_symbol", "multi_edit", "bash_output", "wait", "kill_shell"} {
		if !strings.Contains(names, want) {
			t.Errorf("builtin %q missing from: %s", want, names)
		}
	}
}

func mustGet(r *tool.Registry, name string) tool.Tool {
	tl, ok := r.Get(name)
	if !ok {
		panic("tool not registered: " + name)
	}
	return tl
}
