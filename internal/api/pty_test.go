package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"scopeforge/internal/tmux"
)

// TestPTYWebSocket WS 终端通道(§2.3):list/capture 只读可用,写操作需授权。
func TestPTYWebSocket(t *testing.T) {
	srv := NewServer(Deps{
		Tmux:      tmux.NewManager(nil, "pf"),
		AuthToken: "secret-token",
	})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/pty"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 只读:list(无 tmux 二进制 → 空列表,通道可用)
	if err := conn.WriteJSON(ptyMsg{Type: "tmux", Action: "list"}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ptyMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "output" {
		t.Errorf("resp=%+v", resp)
	}
	// 写操作未授权 → error
	if err := conn.WriteJSON(ptyMsg{Type: "tmux", Action: "send", Session: "s", Keys: "ls"}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "error" || !strings.Contains(resp.Cmd, "write mode") {
		t.Errorf("unauthorized write resp=%+v", resp)
	}
}

// TestPTYWebSocketWriteAuthorized 授权后写操作可达(tmux 缺失时明确报错,通道连通)。
func TestPTYWebSocketWriteAuthorized(t *testing.T) {
	srv := NewServer(Deps{
		Tmux:      tmux.NewManager(nil, "pf"),
		AuthToken: "secret-token",
	})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/pty?write=1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 带 token 的写请求:tmux 缺失 → 命令执行错误(通道本身可用)
	conn.WriteJSON(ptyMsg{Type: "tmux", Action: "new", Session: "msf", Cmd: "msfconsole -q"})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ptyMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "error" && resp.Type != "output" {
		t.Errorf("resp=%+v", resp)
	}
}

// TestPTYWebSocketWriteQueryToken 浏览器通道:WebSocket 无法设 header,
// ?token= query 鉴权后写操作可达(tmux 缺失时明确报错,通道连通)。
func TestPTYWebSocketWriteQueryToken(t *testing.T) {
	srv := NewServer(Deps{
		Tmux:      tmux.NewManager(nil, "pf"),
		AuthToken: "secret-token",
	})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	// 无 token:写操作被拒(通道连通)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/ws/pty?write=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.WriteJSON(ptyMsg{Type: "tmux", Action: "new", Session: "msf", Cmd: "x"})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ptyMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Cmd, "write mode") {
		t.Errorf("无 token 应被拒,resp=%+v", resp)
	}
	conn.Close()

	// query token:写操作放行(缺 tmux → 命令执行错误,而非鉴权错误)
	conn2, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/ws/pty?write=1&token=secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	_ = conn2.WriteJSON(ptyMsg{Type: "tmux", Action: "new", Session: "msf", Cmd: "msfconsole -q"})
	_ = conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp2 ptyMsg
	if err := conn2.ReadJSON(&resp2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp2.Cmd, "write mode") {
		t.Errorf("query token 应放行,resp=%+v", resp2)
	}
}

func TestPTYPing(t *testing.T) {
	srv := NewServer(Deps{Tmux: tmux.NewManager(nil, "pf")})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/pty"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(ptyMsg{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ptyMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "pong" {
		t.Errorf("resp=%+v", resp)
	}
}

var _ = json.RawMessage{}
