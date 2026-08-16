package sandbox

// 容器化 bash 工具(Cairn 模式:平台本机跑,Agent 命令在挑战容器内执行)。
//
// 与上游宿主机 bash 工具同名同 Schema,注册表替换后模型无感;
// 命令经 `docker exec <container> timeout -s KILL <secs> sh -c <cmd>`
// 在"一题一容器"的攻击镜像内执行(sqlmap/nmap 等工具只存在于容器)。
//
// challengeID 由 executor 在工具调用前注入 ctx(ContextWithChallenge),
// 容器懒创建(EnsureContainer)并随 challenge 结束清理(RemoveForChallenge)。

import (
	"context"
	"encoding/json"
	"fmt"

	"scopeforge/internal/reasonix/tool"
)

// ctxKey 是 ctx 值键。
type ctxKey int

const challengeKey ctxKey = iota

// ContextWithChallenge 把 challengeID 注入工具执行 ctx(executor 调用前)。
func ContextWithChallenge(ctx context.Context, challengeID string) context.Context {
	return context.WithValue(ctx, challengeKey, challengeID)
}

// ChallengeFromContext 读取注入的 challengeID(空 = 未注入)。
func ChallengeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(challengeKey).(string)
	return v
}

// bashSchema 与原宿主机 bash 工具一致(模型无感;后台任务参数保留但容器版暂不支持,
// schema 描述中明确提示,避免模型照抄 run_in_background 后报错)。
var bashSchema = json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute"},"run_in_background":{"type":"boolean","description":"NOT supported in container mode: omit this parameter and run the command synchronously."},"preserve_background_processes":{"type":"boolean","description":"NOT supported in container mode: omit this parameter."}},"required":["command"]}`)

// maxOutput 单条命令输出上限(防大输出占满上下文,参考上游 SnipHint 8K 头尾)。
const maxOutput = 32768

// ContainerBash 是容器化 bash 工具。
type ContainerBash struct {
	Mgr *Docker
}

// Name 与原工具一致(注册表替换)。
func (ContainerBash) Name() string { return "bash" }

// Description 说明执行位置,提示模型工具面在攻击容器内。
func (ContainerBash) Description() string {
	return "Execute a command in the challenge's Kali attack container (image: scopeforge-attacker). " +
		"Preinstalled tools include: sqlmap, nmap, nikto, dirsearch, ffuf, gobuster, hydra, netcat, curl, wget, python3, tmux, jq, dnsutils, whois, chromium (agent-browser). " +
		"Prefer these installed tools over writing custom code; for SQL injection you can run sqlmap directly via bash, e.g. sqlmap -u URL --batch. " +
		"File operations should prefer dedicated tools (grep/read_file/etc)."
}

// Schema 与原 bash 一致。
func (ContainerBash) Schema() json.RawMessage { return bashSchema }

// ReadOnly bash 效果无法静态推断,保持保守(false)。
func (ContainerBash) ReadOnly() bool { return false }

type bashArgs struct {
	Command                     string `json:"command"`
	RunInBackground             bool   `json:"run_in_background"`
	PreserveBackgroundProcesses bool   `json:"preserve_background_processes"`
}

// Execute 在挑战容器内执行命令。
func (b ContainerBash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p bashArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Command == "" {
		return "", fmt.Errorf("command is required")
	}
	if b.Mgr == nil {
		return "", fmt.Errorf("sandbox bash: manager not configured")
	}
	if p.RunInBackground {
		return "", fmt.Errorf("sandbox bash: run_in_background 暂不支持(容器模式请同步执行)——请移除 run_in_background 参数后直接执行该命令(工具会同步等待结果)")
	}
	cid := ChallengeFromContext(ctx)
	if cid == "" {
		return "", fmt.Errorf("sandbox bash: challenge context missing(executor 未注入 challenge_id)")
	}
	ct, err := b.Mgr.EnsureContainer(ctx, cid)
	if err != nil {
		return "", fmt.Errorf("sandbox bash: %w", err)
	}
	out, timedOut, err := b.Mgr.ExecTimeout(ctx, ct.ID, p.Command, b.Mgr.DefaultTimeout)
	if err != nil {
		return "", err
	}
	if timedOut {
		return truncate(out, maxOutput) + "\n[container] 命令超时已终止(> " + b.Mgr.DefaultTimeout.String() + ")", nil
	}
	return truncate(out, maxOutput), nil
}

var _ tool.Tool = ContainerBash{}
