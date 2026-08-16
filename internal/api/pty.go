package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// ptyUpgrader 是 WS 升级器。CheckOrigin 同源校验(§2.3 防跨站截取终端输出):
// Origin host 必须与请求 Host 一致(前端同源连接;跨站 WS 被拒)。
var ptyUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // 非浏览器客户端(CLI/测试)
		}
		o, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return o.Host == r.Host
	},
}

// ptyMsg 是 WS 客户端消息(docs/05 §2.3 终端区):
//
//	{"type":"tmux","action":"capture|send|list|new","session":"msf","keys":"...","cmd":"..."}
//	{"type":"ping"}
//
// 服务端响应:
//
//	{"type":"output","data":"..."} / {"type":"error","msg":"..."} / {"type":"pong"}
type ptyMsg struct {
	Type    string `json:"type"`
	Action  string `json:"action"`
	Session string `json:"session"`
	Keys    string `json:"keys"`
	Cmd     string `json:"cmd"`
	Data    string `json:"data"`
}

// handlePTY 终端 WebSocket 端点(/ws/pty,§2.3 决策:与 SSE 事件流分离)。
// 只读默认:capture/list 无需鉴权;send/new 写操作需 ?write=1 + 鉴权。
// 鉴权双通道:Authorization header(CLI/测试)或 ?token= query(浏览器
// WebSocket 无法设置自定义 header,只能带 query token —— 前端写模式走此通道)。
func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tmux == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tmux unavailable"})
		return
	}
	writeMode := r.URL.Query().Get("write") == "1" &&
		(s.authorized(r) || (s.deps.AuthToken != "" && r.URL.Query().Get("token") == s.deps.AuthToken))
	conn, err := ptyUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 * 1024)

	// 心跳:每 20s ping(WS 重连探测,§2.5)
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ping.C:
				_ = conn.WriteJSON(ptyMsg{Type: "pong"})
			}
		}
	}()

	for {
		var msg ptyMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case "ping":
			_ = conn.WriteJSON(ptyMsg{Type: "pong"})
		case "tmux":
			out, err := s.tmuxAction(msg, writeMode)
			if err != nil {
				_ = conn.WriteJSON(ptyMsg{Type: "error", Action: msg.Action, Session: msg.Session, Cmd: err.Error()})
				continue
			}
			_ = conn.WriteJSON(ptyMsg{Type: "output", Action: msg.Action, Session: msg.Session, Data: out})
		default:
			_ = conn.WriteJSON(ptyMsg{Type: "error", Cmd: "unknown message type"})
		}
	}
}

// tmuxAction 执行 tmux 动作(§2.3:新会话/发键/截屏/列表/杀死)。
func (s *Server) tmuxAction(msg ptyMsg, writeMode bool) (string, error) {
	m := s.deps.Tmux
	switch msg.Action {
	case "list":
		sessions, err := m.List(context.Background())
		if err != nil {
			return "", err
		}
		out := ""
		for _, sess := range sessions {
			out += sess.Name + "\n"
		}
		return out, nil
	case "capture":
		if msg.Session == "" {
			return "", fmt.Errorf("session required")
		}
		return m.Capture(context.Background(), msg.Session)
	case "new":
		if !writeMode {
			return "", fmt.Errorf("write mode requires authorization")
		}
		if msg.Session == "" || msg.Cmd == "" {
			return "", fmt.Errorf("session and cmd required")
		}
		if err := m.NewSession(context.Background(), msg.Session, msg.Cmd); err != nil {
			return "", err
		}
		return fmt.Sprintf("session %s created", msg.Session), nil
	case "send":
		if !writeMode {
			return "", fmt.Errorf("write mode requires authorization")
		}
		if msg.Session == "" || msg.Keys == "" {
			return "", fmt.Errorf("session and keys required")
		}
		if err := m.SendKeys(context.Background(), msg.Session, msg.Keys); err != nil {
			return "", err
		}
		return "sent", nil
	default:
		return "", fmt.Errorf("unknown action %q", msg.Action)
	}
}
