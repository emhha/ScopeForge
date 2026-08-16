// Package kb 是漏洞知识库(docs/04 §6 知识注入与漏洞检索)。
//
// 三分寸(⚑ blueprint §(f)):
//   - 方法论: System Prompt + skill 卡(一次性,常驻)
//   - 事实性知识: vulnerability_search 工具按需检索(识别产品/版本后)
//   - 赛题先验: 插件目录 plugins/ 热插拔(仅当可验证,绝不进核心)
//
// 语义(⚑ 弱反证,§6.2):
//   - 检索空结果 = 弱反证(不是"没有漏洞"),不能据此收工
//   - 检索结果 = Hypothesis,必须回目标侧验证(不能直接当事实)
//   - 未经验证的条目标记 Verified=false(调用方按 hypothesis 对待)
//
// 质量纪律(§6.3):内置索引经 go:embed 固化;CI 测试校验条目完整性与断链
// (POC URL 合法);插件目录增删不重启生效(mtime 增量重扫)。
package kb

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Vuln 是一条漏洞知识条目。
type Vuln struct {
	CVE              string   `json:"cve" yaml:"cve"`
	Severity         string   `json:"severity" yaml:"severity"` // critical | high | medium | low
	Summary          string   `json:"summary" yaml:"summary"`
	Product          string   `json:"product" yaml:"product"`
	Versions         []string `json:"versions,omitempty" yaml:"versions,omitempty"` // 受影响版本(空 = 未知)
	PocURL           string   `json:"poc_url,omitempty" yaml:"poc_url,omitempty"`
	ExploitAvailable bool     `json:"exploit_available" yaml:"exploit_available"`
	Source           string   `json:"source" yaml:"source"`
	Verified         bool     `json:"verified" yaml:"verified"` // 验证状态;未验证 → hypothesis
}

//go:embed data/cve-index.json
var builtinIndex []byte

// Index 是知识库索引。
type Index struct {
	mu          sync.RWMutex
	builtin     []Vuln
	plugins     []Vuln
	pluginsDir  string
	pluginFiles map[string]time.Time // 文件 → mtime(增量重扫)
	loadedAt    time.Time
	lastScan    time.Time // 插件目录重扫缓存(工具检索节流)
}

// New 构建索引:加载内置 + 扫描插件目录(不存在则跳过)。
func New(pluginsDir string) (*Index, error) {
	ix := &Index{pluginsDir: pluginsDir, pluginFiles: map[string]time.Time{}}
	if err := ix.loadBuiltin(); err != nil {
		return nil, err
	}
	if pluginsDir != "" {
		if _, err := ix.ReloadPlugins(); err != nil {
			return nil, err
		}
	}
	ix.loadedAt = time.Now()
	return ix, nil
}

func (ix *Index) loadBuiltin() error {
	var entries []Vuln
	if err := json.Unmarshal(builtinIndex, &entries); err != nil {
		return fmt.Errorf("kb: builtin index corrupt: %w", err)
	}
	if err := Validate(entries); err != nil {
		return fmt.Errorf("kb: builtin index invalid: %w", err)
	}
	ix.builtin = entries
	return nil
}

// Add 程序化添加条目(测试用)。
func (ix *Index) Add(v Vuln) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.builtin = append(ix.builtin, v)
}

// ReloadPlugins 重扫插件目录(热插拔:增删改文件不重启生效)。
// 返回本次新增/变更文件数。
func (ix *Index) ReloadPlugins() (int, error) {
	entries, files, err := scanPlugins(ix.pluginsDir)
	if err != nil {
		return 0, err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	changed := 0
	for f, m := range files {
		if old, ok := ix.pluginFiles[f]; !ok || !old.Equal(m) {
			changed++
		}
	}
	for f := range ix.pluginFiles {
		if _, ok := files[f]; !ok {
			changed++
		}
	}
	ix.plugins = entries
	ix.pluginFiles = files
	ix.lastScan = time.Now()
	return changed, nil
}

// Search 检索漏洞(§6.2):
//   - product/version 精确或子串匹配;keywords 匹配 summary/cve
//   - 空结果 = 弱反证(调用方不得据此收工)
//   - 返回按 severity 排序
func (ix *Index) Search(product, version, keywords string) []Vuln {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	all := append(append([]Vuln{}, ix.builtin...), ix.plugins...)
	var hits []Vuln
	for _, v := range all {
		if !matchAny(v.Product, product) {
			continue
		}
		if version != "" && len(v.Versions) > 0 && !matchVersions(v.Versions, version) {
			continue
		}
		if keywords != "" && !matchKeywords(v, keywords) {
			continue
		}
		hits = append(hits, v)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return severityRank(hits[i].Severity) < severityRank(hits[j].Severity)
	})
	return hits
}

// Format 格式化检索结果(工具输出文本)。
// 空结果带弱反证提示(§6.2 语义)。
func Format(hits []Vuln) string {
	if len(hits) == 0 {
		return "未找到匹配的漏洞条目。注意:检索空结果是弱反证,不代表没有漏洞,不能据此收工;应继续探索验证。"
	}
	var b strings.Builder
	for _, v := range hits {
		fmt.Fprintf(&b, "- %s [%s] %s\n", v.CVE, v.Severity, v.Summary)
		fmt.Fprintf(&b, "  product=%s versions=%v exploit=%v source=%s verified=%v\n",
			v.Product, v.Versions, v.ExploitAvailable, v.Source, v.Verified)
		if v.PocURL != "" {
			fmt.Fprintf(&b, "  poc: %s\n", v.PocURL)
		}
		if !v.Verified {
			b.WriteString("  [hypothesis] 未经验证,必须回目标侧确认后再利用\n")
		}
	}
	return b.String()
}

// Validate 校验条目完整性(CI 断链检测,§6.3)。
func Validate(entries []Vuln) error {
	for i, v := range entries {
		if v.CVE == "" || v.Summary == "" || v.Product == "" || v.Source == "" {
			return fmt.Errorf("entry[%d] %q missing required fields (cve/summary/product/source)", i, v.CVE)
		}
		switch strings.ToLower(v.Severity) {
		case "critical", "high", "medium", "low", "":
		default:
			return fmt.Errorf("entry[%d] %q bad severity %q", i, v.CVE, v.Severity)
		}
		if v.PocURL != "" && !strings.HasPrefix(v.PocURL, "http://") && !strings.HasPrefix(v.PocURL, "https://") {
			return fmt.Errorf("entry[%d] %q bad poc_url %q (broken link)", i, v.CVE, v.PocURL)
		}
	}
	return nil
}

// Count 返回条目总数(诊断)。
func (ix *Index) Count() (builtin, plugins int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.builtin), len(ix.plugins)
}

// scanPlugins 扫描插件目录(json/yaml 单条或数组)。
func scanPlugins(dir string) ([]Vuln, map[string]time.Time, error) {
	var entries []Vuln
	files := map[string]time.Time{}
	stat, err := os.Stat(dir)
	if err != nil || !stat.IsDir() {
		return nil, files, nil // 目录不存在 = 无插件
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".json" && ext != ".yaml" && ext != ".yml" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("kb: read plugin %s: %w", p, err)
		}
		var one Vuln
		var many []Vuln
		if ext == ".json" {
			if err := json.Unmarshal(data, &one); err != nil {
				if err2 := json.Unmarshal(data, &many); err2 != nil {
					return nil, nil, fmt.Errorf("kb: plugin %s: %v", p, err2)
				}
			} else {
				many = []Vuln{one}
			}
		} else {
			if err := yaml.Unmarshal(data, &one); err != nil {
				if err2 := yaml.Unmarshal(data, &many); err2 != nil {
					return nil, nil, fmt.Errorf("kb: plugin %s: %v", p, err2)
				}
			} else {
				many = []Vuln{one}
			}
		}
		if err := Validate(many); err != nil {
			return nil, nil, fmt.Errorf("kb: plugin %s: %w", p, err)
		}
		entries = append(entries, many...)
		if fi, err := os.Stat(p); err == nil {
			files[p] = fi.ModTime()
		}
	}
	return entries, files, nil
}

func matchAny(hay, needle string) bool {
	h := strings.ToLower(hay)
	n := strings.ToLower(needle)
	return n == "" || strings.Contains(h, n) || strings.Contains(n, h)
}

// matchVersions 版本匹配:精确、前缀或同版本族(8.5.20 ↔ 8.5.50 同属 8.5.x)。
func matchVersions(list []string, version string) bool {
	v := strings.ToLower(version)
	for _, s := range list {
		s = strings.ToLower(s)
		if s == v || strings.HasPrefix(v, s) || strings.HasPrefix(s, v) {
			return true
		}
		if versionFamily(v) != "" && versionFamily(v) == versionFamily(s) {
			return true
		}
	}
	return false
}

// versionFamily 返回版本族(去掉最后一段,如 8.5.20 → 8.5;单段返回空)。
func versionFamily(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], ".")
}

func matchKeywords(v Vuln, keywords string) bool {
	kw := strings.ToLower(keywords)
	return strings.Contains(strings.ToLower(v.Summary), kw) ||
		strings.Contains(strings.ToLower(v.CVE), kw) ||
		strings.Contains(strings.ToLower(v.Product), kw)
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	}
	return 4
}
