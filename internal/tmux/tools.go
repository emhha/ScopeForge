package tmux

import (
	"context"
	"encoding/json"
	"fmt"

	"scopeforge/internal/reasonix/tool"
)

// Tools 是 tmux 工具集(docs/04 §2.2,注册进 Registry 的 5 个工具)。
type Tools struct {
	M *Manager
}

// NewTools 构建工具集。
func NewTools(m *Manager) []tool.Tool {
	t := &Tools{M: m}
	return []tool.Tool{
		&tmuxTool{name: "tmux_new_session", desc: "创建交互式 tmux 会话(后台运行)。交互式工具(msfconsole/ssh/sqlmap/stowaway 等)必须走 tmux,禁止裸 subprocess 交互(防卡死/防僵尸)。会话随 challenge 结束销毁。",
			schema: `{"type":"object","properties":{"name":{"type":"string"},"cmd":{"type":"string"}},"required":["name","cmd"]}`, run: t.newSession},
		&tmuxTool{name: "tmux_send_keys", desc: "向 tmux 会话发送按键序列并回车。例如发送交互命令:tmux_send_keys(name=\"msf\", keys=\"use exploit/multi/handler\")。",
			schema: `{"type":"object","properties":{"name":{"type":"string"},"keys":{"type":"string"}},"required":["name","keys"]}`, run: t.sendKeys},
		&tmuxTool{name: "tmux_capture_pane", desc: "截取 tmux 会话当前屏幕输出(控制字符已清洗,受 80 行截断约束)。状态探测与输出读取用。",
			schema: `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`, run: t.capture},
		&tmuxTool{name: "tmux_session_list", desc: "列出本 worker 的全部 tmux 会话(状态探测:每轮开工先查,不假设上一轮会话仍在)。",
			schema: `{"type":"object","properties":{}}`, run: t.list},
		&tmuxTool{name: "tmux_session_kill", desc: "终止指定 tmux 会话(清理交互进程)。",
			schema: `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`, run: t.kill},
	}
}

type tmuxTool struct {
	name   string
	desc   string
	schema string
	run    func(ctx context.Context, args map[string]any) (string, error)
}

func (t *tmuxTool) Name() string        { return t.name }
func (t *tmuxTool) Description() string { return t.desc }
func (t *tmuxTool) Schema() json.RawMessage {
	return json.RawMessage(t.schema)
}
func (t *tmuxTool) ReadOnly() bool {
	switch t.name {
	case "tmux_capture_pane", "tmux_session_list":
		return true
	}
	return false
}

func (t *tmuxTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", fmt.Errorf("%s: bad args: %v", t.name, err)
		}
	}
	return t.run(ctx, m)
}

func (t *Tools) newSession(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	cmd, _ := args["cmd"].(string)
	if err := t.M.NewSession(ctx, name, cmd); err != nil {
		return "", err
	}
	return fmt.Sprintf("tmux 会话已创建: %s (cmd: %s)", name, cmd), nil
}

func (t *Tools) sendKeys(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	keys, _ := args["keys"].(string)
	if err := t.M.SendKeys(ctx, name, keys); err != nil {
		return "", err
	}
	return fmt.Sprintf("已发送按键到 %s: %q", name, keys), nil
}

func (t *Tools) capture(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	out, err := t.M.Capture(ctx, name)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (t *Tools) list(ctx context.Context, args map[string]any) (string, error) {
	sessions, err := t.M.List(ctx)
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "tmux 会话: 无", nil
	}
	out := "tmux 会话:\n"
	for _, s := range sessions {
		out += fmt.Sprintf("- %s (created=%d)\n", s.Name, s.Created)
	}
	return out, nil
}

func (t *Tools) kill(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	if err := t.M.Kill(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("tmux 会话已终止: %s", name), nil
}
