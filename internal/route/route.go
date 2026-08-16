// Package route 是受管路由(内网穿透与流量代理,docs/04 §3.1/3.2)。
//
// 拓扑:
//
//	Worker(容器) ──► 目标外网入口
//	    │
//	    ├─ 单跳:   chisel client → chisel server(宿主) → socks5 → 内网段
//	    ├─ 多跳:   stowaway(逐级 agent,深网段)
//	    └─ 出口:   proxychains 包装命令(nmap/curl 走代理)
//	    └─ 捕获:   内置 HTTP 代理 / mitmproxy(流量记录 + flowRef 审计)
//
// 工具(docs/04 §3.2):
//
//	route_open(proto, target, opts)   // 建立隧道,返回 routeID
//	route_status(routeID)             // 健康检查(每轮开工先查,§5.2 纪律)
//	route_stop(routeID)               // 关闭
//	route_reconnect(routeID)          // 断线重连(指数退避)
//	route_proxychains(routeID, cmd)   // 出口:包装命令走代理
//
// 后端可插拔(真实二进制缺失时自动降级):
//   - socks5(内置,纯 Go):无外部依赖,测试与离线环境兜底
//   - chisel:检测 chisel 二进制;server 宿主侧,client 命令交 agent 在目标侧执行
//   - stowaway:检测二进制(admin 宿主侧)
//   - proxychains4:出口包装(缺失时用环境变量代理,curl/wget 兼容)
package route

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"scopeforge/internal/event"
	"scopeforge/internal/traffic"
)

// Proto 是隧道协议。
type Proto string

const (
	ProtoSocks5   Proto = "socks5"   // 内置 socks5 出口(测试/兜底)
	ProtoChisel   Proto = "chisel"   // chisel 单跳
	ProtoStowaway Proto = "stowaway" // stowaway 多跳
	ProtoHTTP     Proto = "http"     // 内置 HTTP 代理出口(明文捕获)
)

// Route 是一条隧道。
type Route struct {
	ID        string `json:"id"`
	Proto     Proto  `json:"proto"`
	Target    string `json:"target"`     // 目标网段/主机(如 10.10.0.0/24)
	LocalAddr string `json:"local_addr"` // 本地出口地址(如 127.0.0.1:19000)
	Status    string `json:"status"`     // running | stopped | degraded
	Backend   string `json:"backend"`    // 实际后端(socks5|chisel|stowaway)
	Created   int64  `json:"created"`
	ClientCmd string `json:"client_cmd,omitempty"` // 目标侧需执行的命令(如 chisel client)
}

type routeState struct {
	Route
	cmd   *exec.Cmd      // 宿主侧进程(chisel server / stowaway admin)
	proxy *traffic.Proxy // http 出口
	socks *socks5Server  // 内置 socks5
	stop  chan struct{}
}

// Manager 管理隧道生命周期。
type Manager struct {
	mu        sync.Mutex
	routes    map[string]*routeState
	sink      event.Sink
	recorder  *traffic.Recorder
	challenge string
	next      int
}

// Options 是 Manager 构建参数。
type Options struct {
	Sink      event.Sink
	Recorder  *traffic.Recorder // 流量审计(可 nil)
	Challenge string
}

// NewManager 构建 Manager。
func NewManager(opts Options) *Manager {
	return &Manager{routes: map[string]*routeState{}, sink: opts.Sink, recorder: opts.Recorder, challenge: opts.Challenge}
}

// Open 建立隧道。
func (m *Manager) Open(ctx context.Context, proto Proto, target string) (*Route, error) {
	if proto == "" {
		proto = ProtoSocks5
	}
	st := &routeState{stop: make(chan struct{})}
	r := Route{ID: m.newID(), Proto: proto, Target: target, Status: "running", Created: time.Now().Unix()}
	st.Route = r
	switch proto {
	case ProtoSocks5:
		srv, err := startSocks5("127.0.0.1:0", m.recorder, m.challenge)
		if err != nil {
			return nil, fmt.Errorf("route: %w", err)
		}
		st.socks = srv
		st.Route.LocalAddr = srv.addr()
		st.Route.Backend = "socks5"
	case ProtoHTTP:
		px, err := traffic.StartProxy(m.recorder, "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("route: %w", err)
		}
		st.proxy = px
		st.Route.LocalAddr = px.Addr()
		st.Route.Backend = "http-proxy"
	case ProtoChisel:
		info, err := m.startChisel(ctx, target, &st.Route)
		if err != nil {
			return nil, err
		}
		st.cmd = info.cmd
	case ProtoStowaway:
		info, err := m.startStowaway(ctx, target, &st.Route)
		if err != nil {
			return nil, err
		}
		st.cmd = info.cmd
	default:
		return nil, fmt.Errorf("route: unsupported proto %q (socks5|http|chisel|stowaway)", proto)
	}
	m.mu.Lock()
	m.routes[st.ID] = st
	m.mu.Unlock()
	m.emit("open", st.Route)
	return &st.Route, nil
}

// Status 健康检查(每轮开工先查,§5.2 状态探测纪律)。
func (m *Manager) Status(id string) (*Route, error) {
	st := m.get(id)
	if st == nil {
		return nil, fmt.Errorf("route: unknown route %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	healthy := false
	switch {
	case st.socks != nil:
		healthy = st.socks.healthy()
	case st.proxy != nil:
		healthy = true
	case st.cmd != nil:
		healthy = isRunning(st.cmd)
	}
	if !healthy {
		st.Status = "stopped"
	}
	out := st.Route
	m.emitLocked("status", out)
	return &out, nil
}

// Stop 关闭隧道。
func (m *Manager) Stop(id string) error {
	st := m.get(id)
	if st == nil {
		return fmt.Errorf("route: unknown route %q", id)
	}
	m.mu.Lock()
	delete(m.routes, id)
	close(st.stop)
	if st.socks != nil {
		st.socks.close()
	}
	if st.proxy != nil {
		st.proxy.Close()
	}
	killAndWait(st.cmd)
	st.Status = "stopped"
	out := st.Route
	m.mu.Unlock()
	m.emit("stop", out)
	return nil
}

// Reconnect 断线重连(指数退避 1s→2s→…→30s 封顶)。
func (m *Manager) Reconnect(ctx context.Context, id string) (*Route, error) {
	st := m.get(id)
	if st == nil {
		return nil, fmt.Errorf("route: unknown route %q", id)
	}
	m.mu.Lock()
	killAndWait(st.cmd)
	st.cmd = nil
	if st.socks != nil {
		st.socks.close()
		st.socks = nil
	}
	if st.proxy != nil {
		st.proxy.Close()
		st.proxy = nil
	}
	m.mu.Unlock()
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		var err error
		m.mu.Lock()
		switch st.Proto {
		case ProtoSocks5:
			var srv *socks5Server
			srv, err = startSocks5("127.0.0.1:0", m.recorder, m.challenge)
			if err == nil {
				st.socks = srv
				st.Route.LocalAddr = srv.addr()
			}
		case ProtoHTTP:
			var px *traffic.Proxy
			px, err = traffic.StartProxy(m.recorder, "127.0.0.1:0")
			if err == nil {
				st.proxy = px
				st.Route.LocalAddr = px.Addr()
			}
		case ProtoChisel:
			var info *procInfo
			info, err = m.startChisel(ctx, st.Target, &st.Route)
			if err == nil {
				st.cmd = info.cmd
			}
		case ProtoStowaway:
			var info *procInfo
			info, err = m.startStowaway(ctx, st.Target, &st.Route)
			if err == nil {
				st.cmd = info.cmd
			}
		}
		if err == nil {
			st.Status = "running"
			out := st.Route
			m.mu.Unlock()
			m.emit("reconnect", out)
			return &out, nil
		}
		m.mu.Unlock()
		if attempt >= 5 {
			m.mu.Lock()
			st.Status = "degraded"
			m.mu.Unlock()
			return nil, fmt.Errorf("route: reconnect failed after %d attempts: %w", attempt, err)
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// CloseAll 关闭全部(challenge 销毁联动,§2.3 纪律)。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.routes))
	for id := range m.routes {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}

// State 返回全部隧道(状态探测)。
func (m *Manager) State() []Route {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Route, 0, len(m.routes))
	for _, st := range m.routes {
		out = append(out, st.Route)
	}
	return out
}

// ProxychainsConf 生成 proxychains 配置文件(指向本路由出口)。
func (m *Manager) ProxychainsConf(id string) (string, error) {
	st := m.get(id)
	if st == nil {
		return "", fmt.Errorf("route: unknown route %q", id)
	}
	if st.socks == nil && st.cmd == nil {
		return "", fmt.Errorf("route %s: no socks5 endpoint (proto=%s)", id, st.Proto)
	}
	addr := st.Route.LocalAddr
	return proxychainsConf(addr), nil
}

// TargetCmd 返回目标侧需执行的客户端命令(如 chisel client)。
func (m *Manager) TargetCmd(id string) (string, error) {
	st := m.get(id)
	if st == nil {
		return "", fmt.Errorf("route: unknown route %q", id)
	}
	if st.Route.ClientCmd == "" {
		return "", fmt.Errorf("route %s: no target-side command for proto=%s", id, st.Proto)
	}
	return st.Route.ClientCmd, nil
}

func (m *Manager) get(id string) *routeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[id]
}

func (m *Manager) newID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	return fmt.Sprintf("r%d", m.next)
}

func (m *Manager) emit(action string, r Route) {
	m.emitLocked(action, r)
}

// emitLocked 发出路由事件(调用方可不持锁;内部只读参数副本)。
func (m *Manager) emitLocked(action string, r Route) {
	if m.sink == nil {
		return
	}
	m.sink.Emit(event.Event{Kind: event.KindRoute, ChallengeID: m.challenge,
		Payload: map[string]any{"action": action, "route_id": r.ID, "proto": string(r.Proto),
			"target": r.Target, "local_addr": r.LocalAddr, "status": r.Status}})
}

// killAndWait 终止进程并回收(防僵尸,docs/04 §2.3 纪律)。
func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// BinaryAvailable 检测外部二进制可用性(doctor 用)。
func BinaryAvailable(ctx context.Context, name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
