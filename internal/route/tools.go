package route

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"scopeforge/internal/reasonix/tool"
)

// Tools 是受管路由工具集(docs/04 §3.2)。
type Tools struct {
	M *Manager
}

// NewTools 构建工具集。
func NewTools(m *Manager) []tool.Tool {
	t := &Tools{M: m}
	return []tool.Tool{
		&routeTool{name: "route_open", desc: "建立内网穿透隧道,返回 route_id 与本地出口地址(socks5)。proto=socks5 为内置出口(无外部依赖);proto=chisel/stowaway 需要宿主安装对应二进制,返回目标侧需执行的 client 命令。拿到立足点后:route_open → 目标侧执行 client 命令 → 经出口访问内网段。",
			schema: `{"type":"object","properties":{"proto":{"type":"string","enum":["socks5","http","chisel","stowaway"]},"target":{"type":"string","description":"目标网段/主机(如 10.10.0.0/24 或 chisel server 地址)"}},"required":["proto"]}`, run: t.open},
		&routeTool{name: "route_status", desc: "隧道健康检查。每轮开工先查全部路由状态,不假设上一轮隧道仍在(断线后需 route_reconnect)。",
			schema: `{"type":"object","properties":{"route_id":{"type":"string"}}}`, run: t.status},
		&routeTool{name: "route_stop", desc: "关闭隧道(释放端口与进程)。challenge 销毁时自动全部关闭。",
			schema: `{"type":"object","properties":{"route_id":{"type":"string"}},"required":["route_id"]}`, run: t.stop},
		&routeTool{name: "route_reconnect", desc: "断线重连(指数退避 1s→30s,最多 5 次)。隧道健康检查失败后调用。",
			schema: `{"type":"object","properties":{"route_id":{"type":"string"}},"required":["route_id"]}`, run: t.reconnect},
		&routeTool{name: "route_proxychains", desc: "让命令走代理出口执行(如 nmap/curl 访问内网段):优先 proxychains4,缺失时用环境变量代理(curl/wget 兼容)。例:route_proxychains(route_id=\"r1\", cmd=\"curl http://10.10.0.2/page\")。",
			schema: `{"type":"object","properties":{"route_id":{"type":"string"},"cmd":{"type":"string"}},"required":["route_id","cmd"]}`, run: t.proxychains},
	}
}

type routeTool struct {
	name   string
	desc   string
	schema string
	run    func(ctx context.Context, args map[string]any) (string, error)
}

func (t *routeTool) Name() string        { return t.name }
func (t *routeTool) Description() string { return t.desc }
func (t *routeTool) Schema() json.RawMessage {
	return json.RawMessage(t.schema)
}
func (t *routeTool) ReadOnly() bool { return t.name == "route_status" }

func (t *routeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", fmt.Errorf("%s: bad args: %v", t.name, err)
		}
	}
	return t.run(ctx, m)
}

func (t *Tools) open(ctx context.Context, args map[string]any) (string, error) {
	proto := ProtoSocks5
	if v, ok := args["proto"].(string); ok && v != "" {
		proto = Proto(v)
	}
	target, _ := args["target"].(string)
	r, err := t.M.Open(ctx, proto, target)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("隧道已建立: route_id=%s proto=%s 本地出口=%s", r.ID, r.Proto, r.LocalAddr)
	if r.ClientCmd != "" {
		out += fmt.Sprintf("\n目标侧需执行(在目标 tmux 会话中): %s", r.ClientCmd)
	}
	return out, nil
}

func (t *Tools) status(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["route_id"].(string)
	if id == "" {
		// 无参数:列出全部(状态探测纪律)
		routes := t.M.State()
		if len(routes) == 0 {
			return "路由: 无", nil
		}
		out := "路由状态:\n"
		for _, r := range routes {
			out += fmt.Sprintf("- %s proto=%s target=%s local=%s status=%s\n", r.ID, r.Proto, r.Target, r.LocalAddr, r.Status)
		}
		return out, nil
	}
	r, err := t.M.Status(id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("route %s: proto=%s target=%s local=%s status=%s", r.ID, r.Proto, r.Target, r.LocalAddr, r.Status), nil
}

func (t *Tools) stop(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["route_id"].(string)
	if err := t.M.Stop(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("隧道已关闭: %s", id), nil
}

func (t *Tools) reconnect(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["route_id"].(string)
	r, err := t.M.Reconnect(ctx, id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("隧道已重连: %s 新出口=%s", r.ID, r.LocalAddr), nil
}

func (t *Tools) proxychains(ctx context.Context, args map[string]any) (string, error) {
	id, _ := args["route_id"].(string)
	cmdText, _ := args["cmd"].(string)
	if cmdText == "" {
		return "", fmt.Errorf("route_proxychains: cmd required")
	}
	conf, err := t.M.ProxychainsConf(id)
	if err != nil {
		return "", err
	}
	// 优先 proxychains4
	if BinaryAvailable(ctx, "proxychains4") {
		f, err := os.CreateTemp("", "proxychains-*.conf")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(conf); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		c := exec.CommandContext(ctx, "proxychains4", "-f", f.Name(), "sh", "-c", cmdText)
		out, err := c.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("proxychains4: %v\n%s", err, truncateOut(out))
		}
		return truncateOut(out), nil
	}
	// 降级:环境变量代理(curl 兼容;socks5 经 ALL_PROXY,socks5h 远端解析)
	addr := t.M.getAddr(id)
	if addr == "" {
		return "", fmt.Errorf("route_proxychains: no endpoint for route %s", id)
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmdText)
	c.Env = append(os.Environ(),
		"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", // 清空继承值,防 http 协议连 socks5 端口
		"ALL_PROXY=socks5h://"+addr,
		"all_proxy=socks5h://"+addr,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("proxy-run: %v\n%s", err, truncateOut(out))
	}
	return truncateOut(out), nil
}

// getAddr 取路由出口地址(内部)。
func (m *Manager) getAddr(id string) string {
	st := m.get(id)
	if st == nil {
		return ""
	}
	return st.Route.LocalAddr
}

func truncateOut(b []byte) string {
	s := string(b)
	if len(s) > 32*1024 {
		return s[:32*1024] + "\n...[output truncated]..."
	}
	return strings.TrimRight(s, "\n")
}
