package semantic

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/memory"
)

const MaxBundleBytes = 1 << 30

type Asset struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // index, model, runtime
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	Protocol     int     `json:"protocol"`
	Platform     string  `json:"platform"`
	Version      string  `json:"version"`
	ModelVersion string  `json:"model_version"`
	Assets       []Asset `json:"assets"`
}
type ComponentStatus struct {
	Available      bool   `json:"available"`
	Installed      bool   `json:"installed"`
	ModelInstalled bool   `json:"model_installed"`
	Installing     bool   `json:"installing"`
	Stage          string `json:"stage"`
	Downloaded     int64  `json:"downloaded"`
	Total          int64  `json:"total"`
	Error          string `json:"error,omitempty"`
	Platform       string `json:"platform"`
	Version        string `json:"version,omitempty"`
	ModelVersion   string `json:"model_version,omitempty"`
	ProbeMS        int64  `json:"probe_ms,omitempty"`
}
type Installation struct {
	Directory string   `json:"directory"`
	Manifest  Manifest `json:"manifest"`
	Local     bool     `json:"local"`
	ProbeMS   int64    `json:"probe_ms"`
}
type Installer struct {
	mu            sync.Mutex
	root, catalog string
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	status        ComponentStatus
}

func NewInstaller(root, catalog string) *Installer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Installer{root: root, catalog: catalog, ctx: ctx, cancel: cancel}
}
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }
func WorkerName() string {
	if runtime.GOOS == "windows" {
		return "worker.exe"
	}
	return "worker"
}
func RuntimeName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	default:
		return "libonnxruntime.so"
	}
}
func FindCatalog() string {
	exe, _ := os.Executable()
	for _, p := range []string{filepath.Join(filepath.Dir(exe), "semantic", "catalog.json"), filepath.Join("dist", "semantic", "catalog.json")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
func (i *Installer) Close() { i.cancel(); i.wg.Wait() }
func (i *Installer) Installed() (Installation, error) {
	var inst Installation
	raw, err := os.ReadFile(filepath.Join(i.root, "installed.json"))
	if err != nil {
		return inst, err
	}
	if err = json.Unmarshal(raw, &inst); err != nil {
		return inst, err
	}
	if filepath.Base(inst.Directory) != inst.Directory || inst.Directory == "." {
		return inst, errors.New("invalid installation")
	}
	if inst.Manifest.Protocol != ProtocolVersion {
		return inst, errors.New("组件协议版本不匹配，请重新安装")
	}
	if inst.Manifest.Platform != Platform() {
		return inst, errors.New("component platform mismatch")
	}
	for _, asset := range inst.Manifest.Assets {
		if !inst.Local && asset.Kind != "index" {
			continue
		}
		if _, err = os.Stat(filepath.Join(i.root, inst.Directory, asset.Name)); err != nil {
			return inst, err
		}
	}
	return inst, nil
}
func (i *Installer) Paths(local bool) (string, Options, error) {
	inst, err := i.Installed()
	if err != nil {
		return "", Options{}, errors.New("请先安装本地语义组件")
	}
	if local && !inst.Local {
		return "", Options{}, errors.New("请先安装本地 Embedding 模型")
	}
	dir := filepath.Join(i.root, inst.Directory)
	opts := Options{Protocol: ProtocolVersion}
	if local {
		opts.Model = filepath.Join(dir, "model.onnx")
		opts.Tokenizer = filepath.Join(dir, "tokenizer.json")
		opts.Runtime = filepath.Join(dir, RuntimeName())
	}
	return filepath.Join(dir, WorkerName()), opts, nil
}
func (i *Installer) Status() ComponentStatus {
	i.mu.Lock()
	s := i.status
	i.mu.Unlock()
	s.Platform = Platform()
	_, err := os.Stat(i.catalog)
	s.Available = err == nil && i.catalog != ""
	if inst, err := i.Installed(); err == nil {
		s.Installed = true
		s.ModelInstalled = inst.Local
		s.Version = inst.Manifest.Version
		s.ModelVersion = inst.Manifest.ModelVersion
		s.ProbeMS = inst.ProbeMS
	}
	return s
}
func validateManifest(m Manifest, local bool) error {
	if m.Protocol != ProtocolVersion || m.Platform != Platform() || m.Version == "" {
		return errors.New("安装包版本或平台不匹配")
	}
	allowed := map[string]string{WorkerName(): "index", "model.onnx": "model", "tokenizer.json": "model", RuntimeName(): "runtime"}
	seen := map[string]bool{}
	for _, a := range m.Assets {
		if allowed[a.Name] != a.Kind || seen[a.Name] || a.Size <= 0 || a.Size > MaxBundleBytes {
			return errors.New("安装清单无效")
		}
		if b, err := hex.DecodeString(a.SHA256); err != nil || len(b) != 32 {
			return errors.New("安装清单缺少有效哈希")
		}
		seen[a.Name] = true
	}
	if !seen[WorkerName()] {
		return errors.New("安装包缺少本地组件")
	}
	if local && (!seen["model.onnx"] || !seen["tokenizer.json"] || !seen[RuntimeName()] || m.ModelVersion == "") {
		return errors.New("安装包缺少完整模型或运行时")
	}
	return nil
}

// Install consumes only a publisher-supplied local catalog, or a package the
// authenticated operator explicitly uploads. It never executes an Internet manifest.
func (i *Installer) Install(local bool, bundle string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.status.Installing {
		return errors.New("已有安装任务正在运行")
	}
	if i.ctx.Err() != nil {
		return i.ctx.Err()
	}
	if bundle == "" && i.catalog == "" {
		return errors.New("当前发行包未附带组件目录，请导入离线组件包")
	}
	i.status = ComponentStatus{Installing: true, Stage: "准备安装"}
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		if bundle != "" {
			defer os.Remove(bundle)
		}
		err := i.install(local, bundle)
		i.mu.Lock()
		defer i.mu.Unlock()
		i.status.Installing = false
		if err != nil {
			i.status.Error = err.Error()
			i.status.Stage = "安装失败，关键词检索可用"
		} else {
			i.status.Stage = "已安装，尚未改变检索模式"
		}
	}()
	return nil
}

func (i *Installer) install(local bool, bundle string) error {
	var m Manifest
	var archive *zip.ReadCloser
	var catalogDir string
	if bundle != "" {
		var err error
		archive, err = zip.OpenReader(bundle)
		if err != nil {
			return err
		}
		defer archive.Close()
		if len(archive.File) > 16 {
			return errors.New("安装包包含过多文件")
		}
		for _, f := range archive.File {
			if f.Name == "catalog.json" {
				r, err := f.Open()
				if err != nil {
					return err
				}
				err = json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&m)
				r.Close()
				if err != nil {
					return err
				}
			}
		}
	} else {
		raw, err := os.ReadFile(i.catalog)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &m); err != nil {
			return err
		}
		catalogDir = filepath.Dir(i.catalog)
	}
	if err := validateManifest(m, local); err != nil {
		return err
	}
	if err := os.MkdirAll(i.root, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(i.root, "install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	var total int64
	for _, a := range m.Assets {
		if local || a.Kind == "index" {
			total += a.Size
		}
	}
	if free, err := availableDisk(i.root); err != nil {
		return fmt.Errorf("无法检测安装磁盘: %w", err)
	} else if free < uint64(total)+(128<<20) {
		return errors.New("安装磁盘空间不足，请保留组件大小加 128 MiB 的可用空间")
	}
	i.mu.Lock()
	i.status.Total = total
	i.mu.Unlock()
	for _, a := range m.Assets {
		if !local && a.Kind != "index" {
			continue
		}
		if err = i.ctx.Err(); err != nil {
			return err
		}
		i.mu.Lock()
		i.status.Stage = "安装 " + a.Name
		i.mu.Unlock()
		var src io.ReadCloser
		if archive != nil {
			for _, f := range archive.File {
				if f.Name == a.Name {
					if src != nil {
						return errors.New("安装包含重复文件")
					}
					src, err = f.Open()
					if err != nil {
						return err
					}
				}
			}
			if src == nil {
				return fmt.Errorf("安装包缺少 %s", a.Name)
			}
		} else if a.URL != "" {
			u, e := url.Parse(a.URL)
			if e != nil || u.Scheme != "https" || u.User != nil {
				return errors.New("下载地址必须使用 HTTPS")
			}
			req, e := http.NewRequestWithContext(i.ctx, http.MethodGet, a.URL, nil)
			if e != nil {
				return e
			}
			client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if r.URL.Scheme != "https" || len(via) > 10 {
					return errors.New("unsafe redirect")
				}
				return nil
			}}
			res, e := client.Do(req)
			if e != nil {
				return fmt.Errorf("下载 %s 失败: %w", a.Name, e)
			}
			if res.StatusCode != 200 {
				res.Body.Close()
				return fmt.Errorf("下载 %s 失败: HTTP %d", a.Name, res.StatusCode)
			}
			src = res.Body
		} else {
			src, err = os.Open(filepath.Join(catalogDir, a.Name))
			if err != nil {
				return err
			}
		}
		f, e := os.OpenFile(filepath.Join(stage, a.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			src.Close()
			return e
		}
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, h, progressWriter{i: i}), io.LimitReader(src, a.Size+1))
		src.Close()
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if n != a.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), a.SHA256) {
			return fmt.Errorf("%s 长度或 SHA256 校验失败", a.Name)
		}
	}
	if err = os.Chmod(filepath.Join(stage, WorkerName()), 0700); err != nil {
		return err
	}
	probeMS, err := probe(i.ctx, stage, local)
	if err != nil {
		return fmt.Errorf("组件自检失败: %w", err)
	}
	// Version directories let an old running process finish while activation is atomic.
	directory := filepath.Base(stage) + "-ready"
	if err = os.Rename(stage, filepath.Join(i.root, directory)); err != nil {
		return err
	}
	inst := Installation{Directory: directory, Manifest: m, Local: local, ProbeMS: probeMS}
	raw, _ := json.Marshal(inst)
	if err = os.WriteFile(filepath.Join(i.root, "installed.tmp"), raw, 0600); err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(i.root, "installed.tmp"), filepath.Join(i.root, "installed.json")); err != nil {
		os.RemoveAll(filepath.Join(i.root, directory))
		return err
	}
	// The HTTP installation entry requires keyword mode, so no active component
	// retains the previous installation. Best-effort cleanup also handles old
	// interrupted upgrades without touching authority stores or vector databases.
	if entries, err := os.ReadDir(i.root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() != directory && strings.HasPrefix(entry.Name(), "install-") && strings.HasSuffix(entry.Name(), "-ready") {
				_ = os.RemoveAll(filepath.Join(i.root, entry.Name()))
			}
		}
	}
	return nil
}

type progressWriter struct{ i *Installer }

func (p progressWriter) Write(b []byte) (int, error) {
	if err := p.i.ctx.Err(); err != nil {
		return 0, err
	}
	p.i.mu.Lock()
	p.i.status.Downloaded += int64(len(b))
	p.i.mu.Unlock()
	return len(b), nil
}

func probe(ctx context.Context, dir string, local bool) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	opts := Options{Protocol: ProtocolVersion, Database: filepath.Join(dir, "probe.db"), Dimensions: 384}
	if local {
		opts.Model = filepath.Join(dir, "model.onnx")
		opts.Tokenizer = filepath.Join(dir, "tokenizer.json")
		opts.Runtime = filepath.Join(dir, RuntimeName())
	}
	c := NewClient(filepath.Join(dir, WorkerName()), opts)
	start := time.Now()
	_, err := c.Call(ctx, Request{Op: "list", Scope: memory.UserKey{AppName: "probe", UserID: "probe"}})
	if err == nil && local {
		var r Response
		r, err = c.Call(ctx, Request{Op: "embed", Text: "query: 对象归属与权限验证"})
		if err == nil && len(r.Vector) != 384 {
			err = errors.New("模型输出维度错误")
		}
	}
	c.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(dir, "probe.db") + suffix)
	}
	return time.Since(start).Milliseconds(), err
}
