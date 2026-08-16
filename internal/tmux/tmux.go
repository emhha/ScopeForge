// Package tmux 是交互式会话的 PTY 三件套(docs/04 §2)。
//
// msfconsole / ssh / sqlmap 交互模式 / stowaway 都是 TTY 程序,无 tmux 的交互
// shell 会卡死(⚑ 两届"做了 vs 没做"的分水岭)。本包封装:
//
//	tmux_new_session(name, cmd)   // tmux new-session -d -s <ns.name> '<cmd>'
//	tmux_send_keys(name, keys)    // tmux send-keys -t <ns.name> '...' Enter
//	tmux_capture_pane(name)       // tmux capture-pane -t <ns.name> -p
//	tmux_session_list()           // 状态探测(开工先查,§5.4 纪律)
//	tmux_session_kill(name)
//
// 纪律(docs/04 §2.3):
//   - 交互式工具一律走 tmux,禁裸 subprocess 交互
//   - capture 输出截断(默认 80 行)且清洗控制字符(不破坏渲染)
//   - 每个 Worker 有独立 session 命名空间(namespace 前缀)
package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// Runner 执行外部命令(测试注入 fake)。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner 是真实命令 runner。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SessionInfo 是会话状态。
type SessionInfo struct {
	Name    string // 完整会话名(namespace.name)
	Created int64  // 创建时间(秒,无则 0)
}

// 控制字符清洗:ANSI CSI/OSC 与私有序列。
var stripCtrl = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z]|[@-Z\\-_])`)

// Manager 管理 tmux 会话。
type Manager struct {
	runner   Runner
	bin      string // tmux 二进制(默认 "tmux")
	ns       string // 命名空间(worker 维度)
	MaxLines int    // capture 截断行数(默认 80)
}

// NewManager 构建 Manager。runner 为空时用真实 ExecRunner;ns 用于隔离
// 每个 worker 的会话命名空间(会话名 = ns.name)。
func NewManager(runner Runner, ns string) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	if ns == "" {
		ns = "pf"
	}
	return &Manager{runner: runner, bin: "tmux", ns: ns, MaxLines: 80}
}

// Bin 返回 tmux 二进制路径(doctor 检测用)。
func (m *Manager) Bin() string { return m.bin }

// Available 检查 tmux 是否可用。
func (m *Manager) Available(ctx context.Context) bool {
	_, err := m.runner.Run(ctx, m.bin, "-V")
	return err == nil
}

// FullName 组装完整会话名(校验合法字符,防注入)。
func (m *Manager) FullName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("tmux: session name required")
	}
	clean := regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(name, "-")
	if clean != name {
		return "", fmt.Errorf("tmux: invalid session name %q (allowed: [A-Za-z0-9._-])", name)
	}
	return m.ns + "." + name, nil
}

// NewSession 创建会话:tmux new-session -d -s <full> '<cmd>'。
func (m *Manager) NewSession(ctx context.Context, name, cmd string) error {
	full, err := m.FullName(name)
	if err != nil {
		return err
	}
	if cmd == "" {
		return fmt.Errorf("tmux: command required for session %q", name)
	}
	if strings.ContainsAny(cmd, "'") {
		return fmt.Errorf("tmux: command must not contain single quotes (injection guard)")
	}
	_, err = m.runner.Run(ctx, m.bin, "new-session", "-d", "-s", full, cmd)
	return err
}

// SendKeys 发送按键:tmux send-keys -t <full> '<keys>' Enter。
func (m *Manager) SendKeys(ctx context.Context, name, keys string) error {
	full, err := m.FullName(name)
	if err != nil {
		return err
	}
	if strings.ContainsAny(keys, "'") {
		return fmt.Errorf("tmux: keys must not contain single quotes (injection guard)")
	}
	_, err = m.runner.Run(ctx, m.bin, "send-keys", "-t", full, keys, "Enter")
	return err
}

// Capture 截取屏幕:tmux capture-pane -t <full> -p。
// 输出:控制字符清洗 + 行数截断(默认 80 行,保头保尾)。
func (m *Manager) Capture(ctx context.Context, name string) (string, error) {
	full, err := m.FullName(name)
	if err != nil {
		return "", err
	}
	out, err := m.runner.Run(ctx, m.bin, "capture-pane", "-t", full, "-p")
	if err != nil {
		return "", err
	}
	clean := stripCtrl.ReplaceAllString(out, "")
	// 剩余 C0 控制字符(保留 \n \t \r)
	clean = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`).ReplaceAllString(clean, "")
	lines := strings.Split(clean, "\n")
	max := m.MaxLines
	if max <= 0 {
		max = 80
	}
	if len(lines) > max {
		head := lines[:max/2]
		tail := lines[len(lines)-max/2:]
		return strings.Join(append(head, "... [truncated] ..."), "\n") + "\n" + strings.Join(tail, "\n"), nil
	}
	return strings.Join(lines, "\n"), nil
}

// List 列出本命名空间下的会话(tmux list-sessions,过滤 ns 前缀)。
func (m *Manager) List(ctx context.Context) ([]SessionInfo, error) {
	out, err := m.runner.Run(ctx, m.bin, "list-sessions", "-F", "#{session_name}\t#{session_created}")
	if err != nil {
		// 无服务器时 tmux 返回非零 — 视为空列表(状态探测语义)
		return nil, nil
	}
	var infos []SessionInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		name := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(name, m.ns+".") {
			continue
		}
		info := SessionInfo{Name: name}
		if len(parts) == 2 {
			var created int64
			if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &created); err == nil {
				info.Created = created
			}
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// Kill 终止会话。
func (m *Manager) Kill(ctx context.Context, name string) error {
	full, err := m.FullName(name)
	if err != nil {
		return err
	}
	_, err = m.runner.Run(ctx, m.bin, "kill-session", "-t", full)
	return err
}

// KillAll 终止本命名空间全部会话(容器销毁联动,§2.3 纪律)。
func (m *Manager) KillAll(ctx context.Context) error {
	sessions, err := m.List(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, s := range sessions {
		if _, err := m.runner.Run(ctx, m.bin, "kill-session", "-t", s.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
