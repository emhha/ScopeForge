package route

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
)

// procInfo 是外部二进制进程。
type procInfo struct {
	cmd *exec.Cmd
}

// startChisel 宿主侧启动 chisel server(--socks5)。
// chisel 缺失时返回明确错误(doctor 会提示安装);client 命令交 agent 在目标侧 tmux 执行。
func (m *Manager) startChisel(ctx context.Context, target string, r *Route) (*procInfo, error) {
	bin, err := exec.LookPath("chisel")
	if err != nil {
		return nil, fmt.Errorf("route: chisel binary not found (chisel backend unavailable); use proto=socks5 内置出口或安装 chisel")
	}
	port, err := freeTCPPort()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "server", "--socks5", "--port", port)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("route: chisel server start: %w", err)
	}
	r.LocalAddr = "127.0.0.1:" + port
	r.Backend = "chisel"
	// 目标侧客户端命令(agent 在目标 tmux 会话中执行)。
	// 注意:target 是目标侧看到的宿主(chisel server)地址,必须由编排环境
	// 注入真实可达地址;无法推断时留空(工具输出会提示)。
	if target != "" {
		r.ClientCmd = fmt.Sprintf("chisel client %s socks", target)
	} else {
		r.ClientCmd = ""
	}
	return &procInfo{cmd: cmd}, nil
}

// startStowaway 宿主侧启动 stowaway admin(多跳隧道控制端)。
// stowaway 二进制缺失时返回明确错误。
func (m *Manager) startStowaway(ctx context.Context, target string, r *Route) (*procInfo, error) {
	bin, err := exec.LookPath("stowaway")
	if err != nil {
		if _, err2 := exec.LookPath("stowaway_admin"); err2 != nil {
			return nil, fmt.Errorf("route: stowaway binary not found (stowaway backend unavailable); use proto=socks5 内置出口或安装 stowaway")
		}
		bin = "stowaway_admin"
	}
	port, err := freeTCPPort()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "-l", port, "-s", "scopeforge-m2")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("route: stowaway admin start: %w", err)
	}
	r.LocalAddr = "127.0.0.1:" + port
	r.Backend = "stowaway"
	if target != "" {
		r.ClientCmd = fmt.Sprintf("stowaway agent -c %s -s scopeforge-m2", target)
	}
	return &procInfo{cmd: cmd}, nil
}

// isRunning 判断进程是否存活(信号 0 探测,避免 ProcessState 延迟误报)。
func isRunning(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

// freeTCPPort 找一个空闲 TCP 端口(返回字符串形式)。
func freeTCPPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", err
	}
	return port, nil
}

// proxychainsConf 生成 proxychains 配置。
func proxychainsConf(socksAddr string) string {
	var b strings.Builder
	b.WriteString("strict_chain\n")
	b.WriteString("proxy_dns\n")
	b.WriteString("remote_dns_subnet 224\n")
	b.WriteString("tcp_read_time_out 15000\n")
	b.WriteString("tcp_connect_time_out 10000\n")
	b.WriteString("[ProxyList]\n")
	b.WriteString("socks5 " + socksAddr + "\n")
	return b.String()
}
