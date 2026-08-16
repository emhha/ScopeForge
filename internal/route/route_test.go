package route

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"scopeforge/internal/event"
	"scopeforge/internal/traffic"
)

type recSink struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recSink) Emit(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// 模拟"内网"服务:只有经路由出口才能访问的本地服务。
func TestSocks5RouteEndToEnd(t *testing.T) {
	sink := &recSink{}
	rec := traffic.NewRecorder(sink, "c1")
	m := NewManager(Options{Sink: sink, Recorder: rec, Challenge: "c1"})
	defer m.CloseAll()

	// 模拟内网靶机(127.0.0.1 上的服务)
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "inner:%s", r.URL.Path)
	}))
	defer inner.Close()

	r, err := m.Open(context.Background(), ProtoSocks5, "10.10.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if r.Backend != "socks5" || r.LocalAddr == "" {
		t.Fatalf("route=%+v", r)
	}

	// 经 socks5 出口访问"内网"
	innerURL := strings.TrimPrefix(inner.URL, "http://")
	conn, err := net.Dial("tcp", r.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	// 手动 socks5 握手
	fmt.Fprintf(conn, "\x05\x01\x00")
	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil || buf[0] != 5 || buf[1] != 0 {
		t.Fatalf("socks handshake failed: %v %v", buf, err)
	}
	host, port, _ := net.SplitHostPort(innerURL)
	fmt.Fprintf(conn, "\x05\x01\x00\x01")
	ip := net.ParseIP(host).To4()
	conn.Write(ip)
	conn.Write([]byte{byte(portInt(port) >> 8), byte(portInt(port))})
	resp := make([]byte, 10)
	if _, err := conn.Read(resp); err != nil || resp[1] != 0 {
		t.Fatalf("socks connect failed: %v %v", resp, err)
	}
	fmt.Fprintf(conn, "GET /pivot HTTP/1.0\r\n\r\n")
	out := make([]byte, 4096)
	n, _ := conn.Read(out)
	if !strings.Contains(string(out[:n]), "inner:/pivot") {
		t.Errorf("tunneled response=%q", out[:n])
	}
	conn.Close()

	// 健康检查
	st, err := m.Status(r.ID)
	if err != nil || st.Status != "running" {
		t.Errorf("status=%+v err=%v", st, err)
	}
	// 流量审计记录(CONNECT)
	flows := rec.Flows()
	found := false
	for _, f := range flows {
		if f.Method == "CONNECT" && strings.Contains(f.Host, host) {
			found = true
		}
	}
	if !found {
		t.Errorf("CONNECT flow not recorded: %+v", flows)
	}
}

func TestSocks5DomainConnect(t *testing.T) {
	m := NewManager(Options{Sink: event.Discard, Challenge: "c1"})
	defer m.CloseAll()
	r, err := m.Open(context.Background(), ProtoSocks5, "")
	if err != nil {
		t.Fatal(err)
	}
	// 域名 ATYP=3 连接(用 127.0.0.1 服务)
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer inner.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(inner.URL, "http://"))
	conn, err := net.Dial("tcp", r.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "\x05\x01\x00")
	buf := make([]byte, 2)
	conn.Read(buf)
	// 域名: localhost
	fmt.Fprintf(conn, "\x05\x01\x00\x03\x09localhost")
	conn.Write([]byte{byte(portInt(port) >> 8), byte(portInt(port))})
	resp := make([]byte, 10)
	if _, err := conn.Read(resp); err != nil || resp[1] != 0 {
		t.Fatalf("domain connect failed: %v", resp)
	}
	conn.Close()
}

func TestRouteReconnectAndStop(t *testing.T) {
	sink := &recSink{}
	m := NewManager(Options{Sink: sink, Challenge: "c1"})
	defer m.CloseAll()
	r, err := m.Open(context.Background(), ProtoSocks5, "10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	oldAddr := r.LocalAddr
	// 停止后健康检查应失败
	if err := m.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(r.ID); err == nil {
		t.Error("status after stop should error")
	}
	// 重开新路由
	r2, err := m.Open(context.Background(), ProtoSocks5, "10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	// reconnect 需在路由仍注册时调用 — 重新注册场景用 Reconnect 验证
	if _, err := m.Reconnect(context.Background(), r2.ID); err != nil {
		t.Fatal(err)
	}
	if r2.LocalAddr == oldAddr {
		t.Error("reconnect should rebind address")
	}
	// 事件含 open/reconnect/stop
	kinds := map[string]int{}
	for _, e := range sink.events {
		if e.Kind == event.KindRoute {
			p := e.Payload.(map[string]any)
			kinds[p["action"].(string)]++
		}
	}
	for _, want := range []string{"open", "reconnect", "stop"} {
		if kinds[want] < 1 {
			t.Errorf("route event %q missing: %v", want, kinds)
		}
	}
}

func TestProxychainsConf(t *testing.T) {
	m := NewManager(Options{Sink: event.Discard})
	r, err := m.Open(context.Background(), ProtoSocks5, "")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := m.ProxychainsConf(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "socks5 "+r.LocalAddr) {
		t.Errorf("conf=%q", conf)
	}
	// 未知路由
	if _, err := m.ProxychainsConf("r99"); err == nil {
		t.Error("unknown route should error")
	}
}

func TestRouteHTTPProxy(t *testing.T) {
	sink := &recSink{}
	rec := traffic.NewRecorder(sink, "c1")
	m := NewManager(Options{Sink: sink, Recorder: rec, Challenge: "c1"})
	defer m.CloseAll()
	r, err := m.Open(context.Background(), ProtoHTTP, "")
	if err != nil {
		t.Fatal(err)
	}
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("captured"))
	}))
	defer inner.Close()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(mustURL("http://" + r.LocalAddr))}}
	resp, err := client.Get(inner.URL + "/traffic")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.Flows()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	flows := rec.Flows()
	if len(flows) != 1 || flows[0].Method != "GET" || !strings.Contains(flows[0].Path, "/traffic") {
		t.Errorf("flows=%+v", flows)
	}
}

func TestChiselBackendFallback(t *testing.T) {
	m := NewManager(Options{Sink: event.Discard})
	// 环境无 chisel 时:返回明确错误而非静默(doctor 会提示)
	r, err := m.Open(context.Background(), ProtoChisel, "")
	if err == nil && r == nil {
		t.Fatal("nil result")
	}
	if err != nil {
		if !strings.Contains(err.Error(), "chisel") {
			t.Errorf("unexpected error: %v", err)
		}
	} else {
		// 有 chisel 的环境:验证 client 命令与状态
		if r.ClientCmd == "" || r.Backend != "chisel" {
			t.Errorf("route=%+v", r)
		}
		m.CloseAll()
	}
}

func portInt(p string) int {
	var v int
	fmt.Sscanf(p, "%d", &v)
	return v
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// TestSocks5MetadataBlocked 出口边界:元数据端点永远拒绝(docs/04 §5.3)。
func TestSocks5MetadataBlocked(t *testing.T) {
	m := NewManager(Options{Sink: event.Discard})
	defer m.CloseAll()
	r, err := m.Open(context.Background(), ProtoSocks5, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", r.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "\x05\x01\x00")
	buf := make([]byte, 2)
	conn.Read(buf)
	// CONNECT 169.254.169.254:80
	fmt.Fprintf(conn, "\x05\x01\x00\x01\xa9\xfe\xa9\xfe\x00\x50")
	resp := make([]byte, 10)
	if n, err := conn.Read(resp); err != nil || n < 2 {
		t.Fatalf("read resp: %v %v", err, resp)
	}
	if resp[1] != 0x02 { // 0x02 = rule not allowed
		t.Errorf("metadata connect must be refused, rep=%d", resp[1])
	}
}

// TestSocks5MetadataBlockedDomain 域名形态的元数据端点同样拒绝。
func TestSocks5MetadataBlockedDomain(t *testing.T) {
	m := NewManager(Options{Sink: event.Discard})
	defer m.CloseAll()
	r, err := m.Open(context.Background(), ProtoSocks5, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", r.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "\x05\x01\x00")
	buf := make([]byte, 2)
	conn.Read(buf)
	// CONNECT metadata.google.internal:80(ATYP=3,域名 24 字节)
	fmt.Fprintf(conn, "\x05\x01\x00\x03\x18metadata.google.internal\x00\x50")
	resp := make([]byte, 10)
	if n, err := conn.Read(resp); err != nil || n < 2 {
		t.Fatalf("read resp: %v %v", err, resp)
	}
	if resp[1] != 0x02 {
		t.Errorf("metadata domain connect must be refused, rep=%d", resp[1])
	}
}
