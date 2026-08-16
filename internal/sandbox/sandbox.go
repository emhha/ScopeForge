// Package sandbox 提供"一任务一容器"攻击容器管理(06.14 起用 docker SDK)。
//
// 容器模型(Cairn 对齐,docs/phase1/04 §5.1):容器内 root + Docker 默认
// capabilities(NET_RAW 等,nmap -sS/sqlmap/tcpdump 可用);红线:绝不
// --privileged。任务级常驻:任务启动即 EnsureContainer,任务结束
// RemoveForChallenge,进程强杀靠 serve 启动 CleanupOrphans 收敛。
// worker(调度波次)经 ExecTimeout docker exec 共享同一容器 —— 登录态/
// 中间产物跨 worker 保持(06.14 用户决策修正,非 worker 级容器)。
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Container 是一个容器。
type Container struct {
	ID        string
	Name      string
	Challenge string
	Created   int64
}

// Docker 是容器管理器(docker SDK 客户端)。
type Docker struct {
	cli       *client.Client
	Image     string // 镜像名(默认 scopeforge-attacker)
	MemoryMB  int    // 内存配额(默认 512)
	Pids      int    // pids 上限(默认 512)
	Net       string // 网络(默认 bridge;host 为 Cairn 评测模式显式配置)
	TmpfsSize string // tmpfs 可写区(默认 128m)

	// DefaultTimeout 单条命令默认超时(容器内 timeout 包装,默认 120s)。
	DefaultTimeout time.Duration

	mu sync.Mutex // EnsureContainer 并发保护
}

// New 构建管理器(懒建 client:Available 失败时无副作用)。
func New(cli *client.Client) *Docker {
	d := &Docker{Image: "scopeforge-attacker", MemoryMB: 512, Pids: 512, Net: "bridge", TmpfsSize: "128m", DefaultTimeout: 2 * time.Minute}
	if cli != nil {
		d.cli = cli
	}
	return d
}

// client 懒初始化(环境变量 DOCKER_HOST 等,FromEnv)。
func (d *Docker) client() (*client.Client, error) {
	if d.cli != nil {
		return d.cli, nil
	}
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}
	d.cli = c
	return c, nil
}

// Available 检查 docker 守护进程可用性。
func (d *Docker) Available(ctx context.Context) bool {
	c, err := d.client()
	if err != nil {
		return false
	}
	_, err = c.Ping(ctx)
	return err == nil
}

// ImagePresent 检查镜像是否存在。
func (d *Docker) ImagePresent(ctx context.Context) bool {
	c, err := d.client()
	if err != nil {
		return false
	}
	_, _, err = c.ImageInspectWithRaw(ctx, d.Image)
	return err == nil
}

// BuildImage 从 Dockerfile 构建镜像(上下文 = Dockerfile 所在目录 tar,
// 排除 .git/node_modules/数据库等大文件)。
func (d *Docker) BuildImage(ctx context.Context, dockerfile string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	dir := filepath.Dir(dockerfile)
	rc, err := tarContext(dir)
	if err != nil {
		return fmt.Errorf("sandbox: build context: %w", err)
	}
	defer rc.Close()
	resp, err := c.ImageBuild(ctx, rc, build.ImageBuildOptions{
		Tags:       []string{d.Image},
		Dockerfile: filepath.Base(dockerfile),
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("sandbox: docker build: %w", err)
	}
	defer resp.Body.Close()
	// 流式读取构建输出(错误时带回显)
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return fmt.Errorf("sandbox: docker build output: %w", err)
	}
	out := buf.String()
	if strings.Contains(out, "ERROR") || strings.Contains(out, "failed") {
		return fmt.Errorf("sandbox: docker build failed:\n%s", truncate(out, 2000))
	}
	return nil
}

// tarContext 打包构建上下文(排除大目录)。
func tarContext(dir string) (io.ReadCloser, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	exclude := map[string]bool{".git": true, "node_modules": true, "web": true, "reports": true}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && exclude[filepath.Base(path)] {
				return filepath.SkipDir
			}
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// Create 创建并启动 Executor 容器(docs/04 §5.1 参数契约):
// 容器内 root + Docker 默认 capabilities;显式禁用 --privileged;
// pids / memory / tmpfs 资源限制;host 网络直达本地靶场。
func (d *Docker) Create(ctx context.Context, challengeID string) (*Container, error) {
	if challengeID == "" {
		return nil, fmt.Errorf("sandbox: challenge_id required")
	}
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	name := "pf-" + sanitize(challengeID) + "-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(d.Net),
		Resources: container.Resources{
			Memory:    int64(d.MemoryMB) * 1024 * 1024,
			PidsLimit: ptr(int64(d.Pids)),
		},
		Tmpfs: map[string]string{"/tmp": "rw,size=" + d.TmpfsSize},
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: d.Image,
			Cmd:   []string{"sleep", "infinity"},
			Labels: map[string]string{
				"scopeforge.challenge": challengeID,
			},
			WorkingDir: "/home/pf/workspace",
			Env:        []string{"TZ=Asia/Shanghai"},
		},
		hostCfg, nil, nil, name)
	if err != nil {
		return nil, fmt.Errorf("sandbox: container create: %w", err)
	}
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: container start: %w", err)
	}
	return &Container{ID: created.ID, Name: name, Challenge: challengeID, Created: time.Now().Unix()}, nil
}

// Exec 在容器内执行命令(状态探测,§5.2 纪律)。
func (d *Docker) Exec(ctx context.Context, containerID, cmd string) (string, error) {
	if containerID == "" || cmd == "" {
		return "", fmt.Errorf("sandbox: container_id and cmd required")
	}
	out, _, err := d.execRaw(ctx, containerID, cmd, 0)
	return out, err
}

// ExecTimeout 在容器内执行命令并强制超时(Cairn ManagedProcess 语义)。
// 容器内 `timeout -s KILL <secs> sh -c <cmd>` 包装:超时杀整条命令链,
// 退出码 137 → 超时标记返回(不产生宿主机孤儿进程)。
func (d *Docker) ExecTimeout(ctx context.Context, containerID, cmd string, timeout time.Duration) (string, bool, error) {
	if containerID == "" || cmd == "" {
		return "", false, fmt.Errorf("sandbox: container_id and cmd required")
	}
	if timeout <= 0 {
		timeout = d.DefaultTimeout
	}
	if timeout <= 0 {
		out, err := d.Exec(ctx, containerID, cmd)
		return out, false, err
	}
	out, exitCode, err := d.execRaw(ctx, containerID, cmd, timeout)
	if err != nil {
		return "", false, err
	}
	if exitCode == 137 {
		return truncate(out, 4000), true, nil
	}
	return out, false, nil
}

// execRaw 执行容器命令并返回输出与退出码;timeout>0 时用容器内
// timeout 包装,超时 → exit 137。
func (d *Docker) execRaw(ctx context.Context, containerID, cmd string, timeout time.Duration) (string, int, error) {
	c, err := d.client()
	if err != nil {
		return "", 0, err
	}
	execCmd := []string{"sh", "-c", cmd}
	if timeout > 0 {
		secs := int(timeout.Seconds())
		if secs < 1 {
			secs = 1
		}
		execCmd = []string{"timeout", "-s", "KILL", strconv.Itoa(secs), "sh", "-c", cmd}
	}
	created, err := c.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          execCmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", 0, fmt.Errorf("sandbox: exec create: %w", err)
	}
	hij, err := c.ContainerExecAttach(ctx, created.ID, container.ExecStartOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("sandbox: exec attach: %w", err)
	}
	defer hij.Close()
	// docker exec attach 流带 8 字节复用头(stdout/stderr 复用),
	// 必须 stdcopy 解复用,否则输出带二进制前缀(agent-browser
	// 的 cdp-url 解析因此失败)。
	var outBuf bytes.Buffer
	_, err = stdcopy.StdCopy(&outBuf, &outBuf, hij.Reader)
	if err != nil && !strings.Contains(err.Error(), "unexpected EOF") && !strings.Contains(err.Error(), "closed") {
		return outBuf.String(), 0, fmt.Errorf("sandbox: exec read: %w", err)
	}
	inspect, err := c.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return outBuf.String(), 0, fmt.Errorf("sandbox: exec inspect: %w", err)
	}
	return outBuf.String(), inspect.ExitCode, nil
}

// EnsureContainer 返回该 challenge 的容器,不存在则创建(任务级常驻)。
// 复用:label=scopeforge.challenge 过滤现有容器,有则复用,无则 Create。
// 并发安全(创建与查询串行化)。
func (d *Docker) EnsureContainer(ctx context.Context, challengeID string) (*Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.List(ctx, challengeID) {
		return &Container{ID: id, Challenge: challengeID}, nil
	}
	return d.Create(ctx, challengeID)
}

// Inspect 验证容器参数符合契约(§5.5 验收:无特权/无 cap-add/资源配额)。
// 返回 JSON 摘要字段。
func (d *Docker) Inspect(ctx context.Context, containerID string) (map[string]string, error) {
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	info, err := c.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker inspect: %v", err)
	}
	m := map[string]string{
		"privileged":  strconv.FormatBool(info.HostConfig.Privileged),
		"cap_add":     fmt.Sprintf("%v", info.HostConfig.CapAdd),
		"security_opt": fmt.Sprintf("%v", info.HostConfig.SecurityOpt),
		"memory":      fmt.Sprintf("%d", info.HostConfig.Memory),
		"pids_limit":  fmt.Sprintf("%d", info.HostConfig.PidsLimit),
		"tmpfs":       fmt.Sprintf("%v", info.HostConfig.Tmpfs),
		"network_mode": string(info.HostConfig.NetworkMode),
	}
	return m, nil
}

// Remove 停止并删除容器(销毁 = 环境重置,§5.1)。
func (d *Docker) Remove(ctx context.Context, containerID string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	err = c.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return fmt.Errorf("sandbox: docker rm: %v", err)
	}
	return nil
}

// RemoveForChallenge 清理某 challenge 的全部容器(重置演练)。
func (d *Docker) RemoveForChallenge(ctx context.Context, challengeID string) error {
	for _, id := range d.List(ctx, challengeID) {
		_ = d.Remove(ctx, id)
	}
	return nil
}

// List 列出 challenge 的容器 ID(label=scopeforge.challenge=<id>)。
func (d *Docker) List(ctx context.Context, challengeID string) []string {
	c, err := d.client()
	if err != nil {
		return nil
	}
	list, err := c.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "scopeforge.challenge="+challengeID)),
	})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(list))
	for _, ct := range list {
		ids = append(ids, ct.ID)
	}
	return ids
}

// MetaDataBlocked 语义检查:元数据端点是否被拒绝(配合 ScopeGate 白名单,
// §5.3 验收)。返回 true 表示该地址不应被访问。
func MetaDataBlocked(addr string) bool {
	host := strings.ToLower(strings.TrimSpace(addr))
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == "169.254.169.254" || host == "metadata.google.internal"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

func ptr[T any](v T) *T { return &v }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// CleanupOrphans 删除所有残留攻击容器(serve 启动时调用)。
// 进程被强杀时 RemoveForChallenge 的 defer 不执行,孤儿容器在此收敛;
// 按 label=scopeforge.challenge 过滤全部(不含具体值)。
func (d *Docker) CleanupOrphans(ctx context.Context) int {
	c, err := d.client()
	if err != nil {
		return 0
	}
	list, err := c.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "scopeforge.challenge")),
	})
	if err != nil {
		return 0
	}
	for _, ct := range list {
		_ = c.ContainerRemove(ctx, ct.ID, container.RemoveOptions{Force: true})
	}
	return len(list)
}
