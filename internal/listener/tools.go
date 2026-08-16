package listener

import (
	"context"
	"encoding/json"
	"fmt"

	"scopeforge/internal/reasonix/tool"
)

// Tools 是反连监听器工具集(docs/04 §3.4)。
type Tools struct {
	M *Manager
}

// NewTools 构建工具集。
func NewTools(m *Manager) []tool.Tool {
	t := &Tools{M: m}
	return []tool.Tool{
		&listenerTool{name: "listener_open", desc: "宿主侧开启反连监听器(tcp/http/dns),返回 listener_id 与监听地址。用于命令注入/SSRF 回连验证、RCE 探测:让目标回连到 <addr>。port 省略时自动分配。",
			schema: `{"type":"object","properties":{"proto":{"type":"string","enum":["tcp","http","dns"]},"port":{"type":"integer"}}}`, run: t.open},
		&listenerTool{name: "listener_poll", desc: "轮询监听器的回连记录(来源/方法/路径/数据摘要)。每次调用返回自上次轮询以来的全部记录。",
			schema: `{"type":"object","properties":{"listener_id":{"type":"string"}},"required":["listener_id"]}`, run: t.poll},
		&listenerTool{name: "listener_close", desc: "关闭监听器(清理)。challenge 销毁时自动全部关闭。",
			schema: `{"type":"object","properties":{"listener_id":{"type":"string"}},"required":["listener_id"]}`, run: t.close},
	}
}

type listenerTool struct {
	name   string
	desc   string
	schema string
	run    func(ctx context.Context, args map[string]any) (string, error)
}

func (t *listenerTool) Name() string        { return t.name }
func (t *listenerTool) Description() string { return t.desc }
func (t *listenerTool) Schema() json.RawMessage {
	return json.RawMessage(t.schema)
}
func (t *listenerTool) ReadOnly() bool { return t.name == "listener_poll" }

func (t *listenerTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", fmt.Errorf("%s: bad args: %v", t.name, err)
		}
	}
	return t.run(ctx, m)
}

func (t *Tools) open(ctx context.Context, args map[string]any) (string, error) {
	proto := ProtoTCP
	if v, ok := args["proto"].(string); ok && v != "" {
		proto = Proto(v)
	}
	port, _ := args["port"].(float64)
	l, err := t.M.Open(ctx, proto, int(port))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("监听器已开启: id=%s proto=%s addr=%s (目标回连地址: %s)", l.ID, l.Proto, l.Addr, l.Addr), nil
}

func (t *Tools) poll(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["listener_id"].(string)
	conns, err := t.M.Poll(id)
	if err != nil {
		return "", err
	}
	if len(conns) == 0 {
		return fmt.Sprintf("监听器 %s: 暂无回连记录", id), nil
	}
	out := fmt.Sprintf("监听器 %s 回连记录(%d):\n", id, len(conns))
	for _, c := range conns {
		out += fmt.Sprintf("- %s from %s", c.Proto, c.Remote)
		if c.Method != "" {
			out += fmt.Sprintf(" %s %s", c.Method, c.Path)
		}
		if c.Host != "" {
			out += fmt.Sprintf(" host=%s", c.Host)
		}
		if c.Data != "" {
			out += fmt.Sprintf(" data=%q", c.Data)
		}
		out += "\n"
	}
	return out, nil
}

func (t *Tools) close(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["listener_id"].(string)
	if err := t.M.Close(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("监听器已关闭: %s", id), nil
}
