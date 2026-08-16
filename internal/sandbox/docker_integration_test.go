package sandbox

// 真实 docker 集成验证(Cairn 容器执行链路):
// 需要 scopeforge-attacker 镜像 + 本机 docker;环境不满足时跳过(不阻断 CI)。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestContainerBashRealDocker(t *testing.T) {
	d := New(nil) // 真实 DockerCLI
	ctx := context.Background()
	if !d.Available(ctx) {
		t.Skip("docker 不可用,跳过真实容器验证")
	}
	if !d.ImagePresent(ctx) {
		t.Skip("scopeforge-attacker 镜像缺失,跳过(docker build -f deploy/executor/Dockerfile -t scopeforge-attacker .)")
	}
	// 清理历史残留
	_ = d.RemoveForChallenge(ctx, "sandbox-verify")
	defer d.RemoveForChallenge(ctx, "sandbox-verify")

	b := ContainerBash{Mgr: d}
	// 无 ctx → 报错
	if _, err := b.Execute(ctx, json.RawMessage(`{"command":"echo hi"}`)); err == nil {
		t.Fatal("无 challenge ctx 应报错")
	}
	// 正常:命令进容器执行(工具只存在于攻击镜像)
	out, err := b.Execute(ContextWithChallenge(ctx, "sandbox-verify"), json.RawMessage(`{"command":"sqlmap --version"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "1.10.6") && !strings.Contains(out, "sqlmap") {
		t.Errorf("out=%q,期望容器内 sqlmap 输出", out)
	}
	// 容器已创建(一题一容器)
	ids := d.List(ctx, "sandbox-verify")
	if len(ids) != 1 {
		t.Fatalf("期望 1 个容器,got %v", ids)
	}
	// 权限契约(§5.5):无特权 / 无 cap-add / 资源配额在位
	m, err := d.Inspect(ctx, ids[0])
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if m["privileged"] != "false" {
		t.Errorf("privileged 必须为 false,got %q", m["privileged"])
	}
	if m["cap_add"] != "null" && m["cap_add"] != "[]" {
		t.Errorf("cap_add 必须为空(不超默认集),got %q", m["cap_add"])
	}
	if m["memory"] == "" || m["memory"] == "0" || m["pids_limit"] == "" || m["pids_limit"] == "0" {
		t.Errorf("资源配额必须设置:memory=%q pids=%q", m["memory"], m["pids_limit"])
	}
	// 容器内 root + 默认 caps → nmap SYN 扫描可用(修复前 Operation not permitted)
	// 注:测试容器为默认 bridge,127.0.0.1 无靶场端口;断言重点是 raw socket
	// 权限错误消失(SYN 扫描成功发起),而非端口开放。
	out, err = b.Execute(ContextWithChallenge(ctx, "sandbox-verify"), json.RawMessage(`{"command":"nmap -sS -p 3000 --min-rate 500 127.0.0.1 2>&1"}`))
	if err != nil {
		t.Fatalf("nmap -sS Execute: %v", err)
	}
	if strings.Contains(out, "Operation not permitted") || strings.Contains(out, "Couldn't open a raw socket") {
		t.Errorf("nmap -sS 应可用(高权限模型),out=%q", out)
	}
	if !strings.Contains(out, "Nmap scan report") {
		t.Errorf("nmap -sS 未正常执行,out=%q", out)
	}
	// 浏览器自动化(BreachWeave 方案):agent-browser 冒烟(构建时同款链路)
	out, err = b.Execute(ContextWithChallenge(ctx, "sandbox-verify"), json.RawMessage(`{"command":"agent-browser open about:blank && agent-browser get title && agent-browser close"}`))
	if err != nil {
		t.Fatalf("agent-browser Execute: %v", err)
	}
	if !strings.Contains(out, "about:blank") && !strings.Contains(out, "title") {
		t.Errorf("agent-browser 冒烟异常,out=%q", out)
	}
	// 超时语义:容器内 timeout 包装杀命令
	_, timedOut, err := d.ExecTimeout(ctx, ids[0], "sleep 5", 1) // 1s 超时
	if err != nil || !timedOut {
		t.Fatalf("期望超时标记,timedOut=%v err=%v", timedOut, err)
	}
}
