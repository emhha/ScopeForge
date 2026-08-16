package sandbox

// 容器模式工具面(06.14 阶段 C-3):容器启用时,文件类工具与 web_fetch
// 全部替换为容器内实现(docker exec),宿主机对应工具 RemovePrefix 关闭。
// 参考 Cairn/LuaN1ao 会话级容器化:agent 的读写/网络/命令都在攻击容器内,
// 不存在"LLM 看宿主路径、执行在容器"的路径撕裂。
//
// 路径语义:agent 给出的路径即容器内路径(容器模式无宿主工作区概念);
// 工作目录 /home/pf/workspace(镜像 WORKDIR)。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"scopeforge/internal/reasonix/tool"
)

// ContainerFileTools 是容器版文件工具集(与宿主机 builtin 同名同 Schema,
// 注册表替换后模型无感)。
type ContainerFileTools struct {
	Mgr *Docker
}

// --- 工具实现模板 ---

type ctFileTool struct {
	name        string
	description string
	schema      json.RawMessage
	readOnly    bool
	// build 构造容器内命令(参数已解析)
	build func(args map[string]any) (string, error)
	mgr   *Docker
}

func (t ctFileTool) Name() string        { return t.name }
func (t ctFileTool) Description() string { return t.description }
func (t ctFileTool) Schema() json.RawMessage { return t.schema }
func (t ctFileTool) ReadOnly() bool      { return t.readOnly }

func (t ctFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	cid := ChallengeFromContext(ctx)
	if cid == "" {
		return "", fmt.Errorf("container tool: challenge context missing (executor 未注入)")
	}
	var args map[string]any
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	cmd, err := t.build(args)
	if err != nil {
		return "", err
	}
	ct, err := t.mgr.EnsureContainer(ctx, cid)
	if err != nil {
		return "", fmt.Errorf("container tool: %w", err)
	}
	out, timedOut, err := t.mgr.ExecTimeout(ctx, ct.ID, cmd, 2*time.Minute)
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	if timedOut {
		return truncate(out, 4000) + "\n...[timed out]", nil
	}
	return truncate(out, 32768), nil
}

// Tools 返回容器版工具集(ls/glob/grep/read_file/cat/edit_file/write_file/
// delete_range/move_file/multi_edit/web_fetch)。
func (f *ContainerFileTools) Tools() []tool.Tool {
	str := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []tool.Tool{
		ctFileTool{
			name: "ls", readOnly: true,
			description: "List directory entries in the attack container. Returns names, sizes, and flags (like ls -la).",
			schema: str(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path (default .)"},"recursive":{"type":"boolean","description":"List recursively (like find)"}},"required":[]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				path, _ := a["path"].(string)
				if path == "" {
					path = "."
				}
				if r, _ := a["recursive"].(bool); r {
					return fmt.Sprintf("find %s -maxdepth 4 | head -300", shq(path)), nil
				}
				return fmt.Sprintf("ls -la %s | head -200", shq(path)), nil
			},
		},
		ctFileTool{
			name: "glob", readOnly: true,
			description: "Find files matching a glob pattern in the attack container (like find with wildcards).",
			schema: str(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern"}},"required":["pattern"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["pattern"].(string)
				if p == "" {
					return "", fmt.Errorf("pattern required")
				}
				return fmt.Sprintf("find / -path %s 2>/dev/null | head -100", shq(p)), nil
			},
		},
		ctFileTool{
			name: "grep", readOnly: true,
			description: "Search for a regex pattern in files inside the attack container (like grep -rn).",
			schema: str(`{"type":"object","properties":{"pattern":{"type":"string","description":"Regex pattern"},"path":{"type":"string","description":"Directory or file to search (default .)"}},"required":["pattern"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				pat, _ := a["pattern"].(string)
				if pat == "" {
					return "", fmt.Errorf("pattern required")
				}
				path, _ := a["path"].(string)
				if path == "" {
					path = "."
				}
				return fmt.Sprintf("grep -rn -E %s %s 2>/dev/null | head -200", shq(pat), shq(path)), nil
			},
		},
		ctFileTool{
			name: "read_file", readOnly: true,
			description: "Read a file from the attack container and return its contents (like cat).",
			schema: str(`{"type":"object","properties":{"path":{"type":"string","description":"File path"}},"required":["path"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["path"].(string)
				if p == "" {
					return "", fmt.Errorf("path required")
				}
				return fmt.Sprintf("cat %s | head -c 30000", shq(p)), nil
			},
		},
		ctFileTool{
			name: "cat", readOnly: true,
			description: "Read a file from the attack container (alias of read_file).",
			schema: str(`{"type":"object","properties":{"path":{"type":"string","description":"File path"}},"required":["path"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["path"].(string)
				if p == "" {
					return "", fmt.Errorf("path required")
				}
				return fmt.Sprintf("cat %s | head -c 30000", shq(p)), nil
			},
		},
		ctFileTool{
			name: "edit_file",
			description: "Replace an exact string in a file inside the attack container. Use for targeted edits.",
			schema: str(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["path"].(string)
				old, _ := a["old_string"].(string)
				new, _ := a["new_string"].(string)
				if p == "" || old == "" {
					return "", fmt.Errorf("path and old_string required")
				}
				// python 精确替换(防特殊字符转义问题)
				py := fmt.Sprintf("import pathlib,sys; p=pathlib.Path(%s); s=p.read_text(); assert %s in s, 'old_string not found'; p.write_text(s.replace(%s, %s, 1))",
					pyq(p), pyq(old), pyq(old), pyq(new))
				return fmt.Sprintf("python3 -c %s", shq(py)), nil
			},
		},
		ctFileTool{
			name: "write_file",
			description: "Write content to a file in the attack container (overwrites).",
			schema: str(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"content":{"type":"string","description":"Full content to write"}},"required":["path","content"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["path"].(string)
				content, _ := a["content"].(string)
				if p == "" {
					return "", fmt.Errorf("path required")
				}
				py := fmt.Sprintf("import pathlib; p=pathlib.Path(%s); p.parent.mkdir(parents=True, exist_ok=True); p.write_text(%s)", pyq(p), pyq(content))
				return fmt.Sprintf("python3 -c %s", shq(py)), nil
			},
		},
		ctFileTool{
			name: "delete_range",
			description: "Delete a contiguous text range from a file in the attack container using start/end anchors.",
			schema: str(`{"type":"object","properties":{"path":{"type":"string"},"start_anchor":{"type":"string"},"end_anchor":{"type":"string"}},"required":["path","start_anchor","end_anchor"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				p, _ := a["path"].(string)
				s, _ := a["start_anchor"].(string)
				e, _ := a["end_anchor"].(string)
				if p == "" || s == "" || e == "" {
					return "", fmt.Errorf("path/start_anchor/end_anchor required")
				}
				py := fmt.Sprintf("import pathlib; p=pathlib.Path(%s); t=p.read_text(); i=t.index(%s); j=t.index(%s, i)+len(%s); p.write_text(t[:i]+t[j:])",
					pyq(p), pyq(s), pyq(e), pyq(e))
				return fmt.Sprintf("python3 -c %s", shq(py)), nil
			},
		},
		ctFileTool{
			name: "move_file",
			description: "Move or rename a file inside the attack container.",
			schema: str(`{"type":"object","properties":{"source_path":{"type":"string"},"destination_path":{"type":"string"}},"required":["source_path","destination_path"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				s, _ := a["source_path"].(string)
				d, _ := a["destination_path"].(string)
				if s == "" || d == "" {
					return "", fmt.Errorf("source_path and destination_path required")
				}
				return fmt.Sprintf("mkdir -p $(dirname %s) && mv %s %s", shq(d), shq(s), shq(d)), nil
			},
		},
		ctFileTool{
			name: "web_fetch", readOnly: true,
			description: "Fetch a URL from inside the attack container (curl) and return the response text/headers. Container-side network.",
			schema: str(`{"type":"object","properties":{"url":{"type":"string","description":"Absolute URL"}},"required":["url"]}`),
			mgr:  f.Mgr,
			build: func(a map[string]any) (string, error) {
				u, _ := a["url"].(string)
				if u == "" {
					return "", fmt.Errorf("url required")
				}
				return fmt.Sprintf("curl -sS -m 20 -i %s | head -c 30000", shq(u)), nil
			},
		},
	}
}

// shq 单引号包裹(shell 安全:命令参数经 sh -c 执行)。
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pyq python 字符串字面量。
func pyq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
