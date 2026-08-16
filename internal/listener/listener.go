// Package listener 是反连监听器(docs/04 §3.4)。
//
// 多阶段渗透必需:命令注入/SSRF 回连验证、RCE 探测。
// 宿主侧开监听(tcp/http/dns),轮询回连记录(替代平台回连依赖):
//
//	listener_open(proto, port)    // 返回 listenerID + 监听地址
//	listener_poll(listenerID)     // 轮询回连记录
//	listener_close(listenerID)
//
// 纯 Go 实现,无外部依赖。每条回连记录入 events(kind=listener),审计可见。
package listener

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/event"
)

// Proto 是监听协议。
type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoHTTP Proto = "http"
	ProtoDNS  Proto = "dns"
)

// Connection 是一条回连记录。
type Connection struct {
	TS     int64  `json:"ts"`
	Remote string `json:"remote"`
	Proto  string `json:"proto"`
	Method string `json:"method,omitempty"` // HTTP: GET/POST/...
	Host   string `json:"host,omitempty"`   // HTTP Host 头
	Path   string `json:"path,omitempty"`   // HTTP 请求路径
	Data   string `json:"data,omitempty"`   // 数据摘要(≤512 字符)
}

// Listener 是一个监听器。
type Listener struct {
	ID      string `json:"id"`
	Proto   Proto  `json:"proto"`
	Addr    string `json:"addr"` // 监听地址(worker 回连用)
	Port    int    `json:"port"`
	Created int64  `json:"created"`
}

type listenerState struct {
	Listener
	ln       net.Listener
	conn     *net.UDPConn
	stop     chan struct{}
	mu       sync.Mutex
	conns    []Connection
	maxConns int // 记录上限(防内存膨胀)
}

// Manager 管理监听器。
type Manager struct {
	mu        sync.Mutex
	states    map[string]*listenerState
	sink      event.Sink
	challenge string
	basePort  int
	nextID    int
}

// NewManager 构建 Manager。basePort<=0 时默认 18000(自动分配端口起点)。
func NewManager(sink event.Sink, challengeID string, basePort int) *Manager {
	if basePort <= 0 {
		basePort = 18000
	}
	return &Manager{states: map[string]*listenerState{}, sink: sink, challenge: challengeID, basePort: basePort}
}

// Open 开启监听器。port<=0 时从 basePort 起自动分配。
func (m *Manager) Open(ctx context.Context, proto Proto, port int) (*Listener, error) {
	if proto == "" {
		proto = ProtoTCP
	}
	switch proto {
	case ProtoTCP, ProtoHTTP, ProtoDNS:
	default:
		return nil, fmt.Errorf("listener: unsupported proto %q (tcp|http|dns)", proto)
	}
	if port <= 0 {
		port = m.nextFreePort(proto)
	}
	addr := fmt.Sprintf(":%d", port)
	st := &listenerState{stop: make(chan struct{}), maxConns: 1000}
	switch proto {
	case ProtoDNS:
		uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, fmt.Errorf("listener: %w", err)
		}
		st.conn = uc
		st.Listener = Listener{ID: m.newID(), Proto: proto, Addr: uc.LocalAddr().String(), Port: uc.LocalAddr().(*net.UDPAddr).Port, Created: time.Now().Unix()}
	default:
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listener: %w", err)
		}
		st.ln = ln
		st.Listener = Listener{ID: m.newID(), Proto: proto, Addr: ln.Addr().String(), Port: ln.Addr().(*net.TCPAddr).Port, Created: time.Now().Unix()}
	}
	m.mu.Lock()
	m.states[st.ID] = st
	m.mu.Unlock()
	go m.serve(st)
	m.emit("open", st.Listener, nil)
	return &st.Listener, nil
}

// Poll 返回监听器的回连记录。
func (m *Manager) Poll(id string) ([]Connection, error) {
	st := m.get(id)
	if st == nil {
		return nil, fmt.Errorf("listener: unknown listener %q", id)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Connection, len(st.conns))
	copy(out, st.conns)
	return out, nil
}

// Close 关闭监听器。
func (m *Manager) Close(id string) error {
	st := m.get(id)
	if st == nil {
		return fmt.Errorf("listener: unknown listener %q", id)
	}
	m.mu.Lock()
	delete(m.states, id)
	m.mu.Unlock()
	close(st.stop)
	if st.ln != nil {
		st.ln.Close()
	}
	if st.conn != nil {
		st.conn.Close()
	}
	m.emit("close", st.Listener, nil)
	return nil
}

// CloseAll 关闭全部(challenge 销毁联动,§2.3 纪律)。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
}

// State 返回全部监听器(状态探测,§5.2 纪律)。
func (m *Manager) State() []Listener {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Listener, 0, len(m.states))
	for _, st := range m.states {
		out = append(out, st.Listener)
	}
	return out
}

// serve 处理连接。
func (m *Manager) serve(st *listenerState) {
	switch st.Proto {
	case ProtoDNS:
		m.serveDNS(st)
	default:
		m.serveTCP(st)
	}
}

func (m *Manager) serveTCP(st *listenerState) {
	for {
		conn, err := st.ln.Accept()
		if err != nil {
			select {
			case <-st.stop:
				return
			default:
				continue
			}
		}
		go m.handleTCPConn(st, conn)
	}
}

func (m *Manager) handleTCPConn(st *listenerState, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	c := Connection{TS: time.Now().Unix(), Remote: conn.RemoteAddr().String(), Proto: string(st.Proto)}
	if st.Proto == ProtoHTTP {
		req, err := httpReadRequest(conn)
		if err == nil {
			c.Method = req.Method
			c.Host = req.Host
			c.Path = req.Path
			// 回 HTTP 应答,让客户端请求完整返回(回连验证常见期望)
			fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		}
	} else {
		buf := make([]byte, 4096)
		if n, err := conn.Read(buf); err == nil && n > 0 {
			c.Data = truncate(string(buf[:n]), 512)
		}
	}
	m.record(st, c)
}

func (m *Manager) serveDNS(st *listenerState) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := st.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-st.stop:
				return
			default:
				continue
			}
		}
		c := Connection{TS: time.Now().Unix(), Remote: addr.String(), Proto: "dns"}
		if name, ok := parseDNSQuery(buf[:n]); ok {
			c.Data = "query=" + name
		} else {
			c.Data = truncate(string(buf[:n]), 512)
		}
		m.record(st, c)
	}
}

// record 记录回连(进程内 + events)。
func (m *Manager) record(st *listenerState, c Connection) {
	st.mu.Lock()
	st.conns = append(st.conns, c)
	if st.maxConns > 0 && len(st.conns) > st.maxConns {
		st.conns = st.conns[len(st.conns)-st.maxConns:]
	}
	st.mu.Unlock()
	m.emit("connection", st.Listener, &c)
}

func (m *Manager) emit(action string, l Listener, c *Connection) {
	if m.sink == nil {
		return
	}
	payload := map[string]any{"action": action, "listener_id": l.ID, "proto": string(l.Proto), "addr": l.Addr}
	if c != nil {
		payload["connection"] = c
	}
	m.sink.Emit(event.Event{Kind: event.KindListener, ChallengeID: m.challenge, Payload: payload})
}

func (m *Manager) get(id string) *listenerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[id]
}

func (m *Manager) newID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	return fmt.Sprintf("l%d-%d", time.Now().UnixNano()%1000000, m.nextID)
}

// nextFreePort 从 basePort 起找空闲端口。
func (m *Manager) nextFreePort(proto Proto) int {
	for port := m.basePort; port < m.basePort+1000; port++ {
		if proto == ProtoDNS {
			if uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port}); err == nil {
				uc.Close() // 探测后立即关闭,由 Open 重新绑定
				return port
			}
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		ln.Close()
		return port
	}
	return 0
}

// httpReadRequest 读取 HTTP 请求行与头部。
type httpReq struct{ Method, Host, Path string }

func httpReadRequest(conn net.Conn) (*httpReq, error) {
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &httpReq{Method: parts[0], Path: parts[1]}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimSpace(h)
		if h == "" {
			break
		}
		if i := strings.Index(h, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(h[:i]), "host") {
			req.Host = strings.TrimSpace(h[i+1:])
		}
	}
	return req, nil
}

// parseDNSQuery 提取 DNS 查询中的 QNAME(最小解析,仅标准单查询报文)。
func parseDNSQuery(b []byte) (string, bool) {
	if len(b) < 12 {
		return "", false
	}
	// 检查 QR=0(查询)
	if b[2]&0x80 != 0 {
		return "", false
	}
	// QDCOUNT
	if qd := binary.BigEndian.Uint16(b[4:6]); qd < 1 {
		return "", false
	}
	var labels []string
	i := 12
	for {
		if i >= len(b) {
			return "", false
		}
		l := int(b[i])
		if l == 0 {
			i++
			break
		}
		if l > 63 || i+1+l > len(b) {
			return "", false
		}
		labels = append(labels, string(b[i+1:i+1+l]))
		i += 1 + l
	}
	if len(labels) == 0 {
		return "", false
	}
	// 检查 QTYPE=QCLASS 存在(可选的 4 字节)
	if i+4 > len(b) {
		return strings.Join(labels, "."), true
	}
	return strings.Join(labels, "."), true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
