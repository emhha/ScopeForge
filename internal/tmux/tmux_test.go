package tmux

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner 记录命令并返回预设输出。
type fakeRunner struct {
	mu       chan struct{}
	commands []string
	out      map[string]string
	err      map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{mu: make(chan struct{}, 1), out: map[string]string{}, err: map[string]error{}}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.mu <- struct{}{}
	f.commands = append(f.commands, key)
	<-f.mu
	if e, ok := f.err[key]; ok {
		return "", e
	}
	return f.out[key], nil
}

func (f *fakeRunner) last() string {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	if len(f.commands) == 0 {
		return ""
	}
	return f.commands[len(f.commands)-1]
}

func TestNewSessionCommands(t *testing.T) {
	r := newFakeRunner()
	m := NewManager(r, "w1")
	if err := m.NewSession(context.Background(), "msf", "msfconsole -q"); err != nil {
		t.Fatal(err)
	}
	want := "tmux new-session -d -s w1.msf msfconsole -q"
	if got := r.last(); got != want {
		t.Errorf("command=%q want %q", got, want)
	}
}

func TestSendKeysAndCapture(t *testing.T) {
	r := newFakeRunner()
	r.out["tmux capture-pane -t w1.msf -p"] = "msf6 > use exploit/multi/handler\n"
	m := NewManager(r, "w1")
	if err := m.SendKeys(context.Background(), "msf", "use exploit/multi/handler"); err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "tmux send-keys -t w1.msf use exploit/multi/handler Enter" {
		t.Errorf("send-keys command=%q", got)
	}
	out, err := m.Capture(context.Background(), "msf")
	if err != nil || !strings.Contains(out, "msf6 > use exploit") {
		t.Errorf("capture out=%q err=%v", out, err)
	}
}

func TestNameInjectionGuard(t *testing.T) {
	r := newFakeRunner()
	m := NewManager(r, "w1")
	if err := m.NewSession(context.Background(), "a; rm -rf /", "x"); err == nil {
		t.Error("invalid name should error")
	}
	if err := m.NewSession(context.Background(), "ok", "cmd ' injection"); err == nil {
		t.Error("single quote in cmd should error")
	}
	if err := m.SendKeys(context.Background(), "ok", "a'b"); err == nil {
		t.Error("single quote in keys should error")
	}
	if len(r.commands) != 0 {
		t.Errorf("no command should have run, got %v", r.commands)
	}
}

func TestCaptureTruncateAndStrip(t *testing.T) {
	r := newFakeRunner()
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("line " + strings.Repeat("x", 30) + "\n")
	}
	// 混入 ANSI 与 OSC 序列
	raw := "\x1b[31mred\x1b[0m\x1b]0;title\x07 plain\n" + sb.String()
	r.out["tmux capture-pane -t w1.t -p"] = raw
	m := NewManager(r, "w1")
	m.MaxLines = 20
	out, err := m.Capture(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b") {
		t.Error("control sequences not stripped")
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 24 {
		t.Errorf("output not truncated: %d lines", len(lines))
	}
	if !strings.Contains(out, "... [truncated] ...") {
		t.Error("truncation marker missing")
	}
	if !strings.Contains(out, "plain") {
		t.Error("stripped content lost")
	}
}

func TestListFiltersNamespace(t *testing.T) {
	r := newFakeRunner()
	r.out["tmux list-sessions -F #{session_name}\t#{session_created}"] =
		"w1.msf\t1000\nother.sess\t2000\nw1.nmap\t3000\n"
	m := NewManager(r, "w1")
	sessions, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "w1.msf" || sessions[1].Name != "w1.nmap" {
		t.Errorf("sessions=%+v", sessions)
	}
	if sessions[1].Created != 3000 {
		t.Errorf("created=%d", sessions[1].Created)
	}
}

func TestKillAll(t *testing.T) {
	r := newFakeRunner()
	r.out["tmux list-sessions -F #{session_name}\t#{session_created}"] = "w1.a\t1\nw1.b\t2\n"
	m := NewManager(r, "w1")
	if err := m.KillAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := r.commands
	if len(cmds) != 3 || cmds[1] != "tmux kill-session -t w1.a" || cmds[2] != "tmux kill-session -t w1.b" {
		t.Errorf("kill commands=%v", cmds)
	}
}

func TestListNoServer(t *testing.T) {
	r := newFakeRunner()
	r.err["tmux list-sessions -F #{session_name}\t#{session_created}"] = errNoServer
	m := NewManager(r, "w1")
	sessions, err := m.List(context.Background())
	if err != nil || sessions != nil {
		t.Errorf("no-server should yield empty list, got %v err=%v", sessions, err)
	}
}

// errNoServer 模拟 tmux 无服务器错误。
var errNoServer = &runnerErr{"no server running on /tmp/tmux-0/default"}

type runnerErr struct{ msg string }

func (e *runnerErr) Error() string { return e.msg }
