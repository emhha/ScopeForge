// Package config 是 ScopeForge 提供的最小 config shim,替代上游
// reasonix/internal/config 中 skill/memory/plugin 用到的路径与命名辅助函数。
//
// 上游 config 是 TOML 体系(依赖扩散源),docs/02 要求配置聚合为 scopeforge.yaml
// (YAML),因此本包只承载"与配置格式无关"的工具函数,上游文件零修改。
// 差异清单见 docs/upstream-tracking.md。
package config

import (
	"fmt"
	"hash/fnv"
	"unicode/utf8"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var validSkillName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// IsValidSkillName reports whether name is a usable skill identifier.
func IsValidSkillName(name string) bool { return validSkillName.MatchString(name) }

// SkillNameKey normalizes a skill identifier for config comparisons.
func SkillNameKey(name string) string {
	name = strings.TrimSpace(name)
	if !IsValidSkillName(name) {
		return ""
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

// CanonicalSkillPath expands ~ and makes a skill path absolute.
func CanonicalSkillPath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// WorkspaceSlug 把工作区绝对路径折叠为安全的文件名组件(记忆按工作区隔离)。
func WorkspaceSlug(absPath string) string {
	if runtime.GOOS == "windows" {
		absPath = strings.ToLower(absPath)
	}
	slug := strings.NewReplacer(string(os.PathSeparator), "-", "/", "-", "\\", "-", ":", "-").Replace(absPath)
	return boundFilenameComponent(slug, 255)
}

// BoundFilenameComponent 把无界输入折叠为不超过 maxLen 字节的文件名组件。
func BoundFilenameComponent(s string, maxLen int) string {
	return boundFilenameComponent(s, maxLen)
}

func boundFilenameComponent(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	budget := maxLen - 17 // room for "-" + 16 hex digits
	prefix := s[:budget]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s-%016x", prefix, h.Sum64())
}

// ConventionDirs are the parent directories scanned for agent assets.
// 上游为 .reasonix/.agents/.agent/.claude;ScopeForge 仅扫描自有约定目录。
var ConventionDirs = []string{".scopeforge"}

// IsolatedHomeDir returns SCOPEFORGE_HOME when set (isolation for tests/CI).
func IsolatedHomeDir() string {
	return strings.TrimSpace(os.Getenv("SCOPEFORGE_HOME"))
}

// ReasonixHomeDir returns the user home dir for agent assets.
// 上游为 ~/.reasonix;ScopeForge 使用 ~/.scopeforge。
func ReasonixHomeDir() string {
	if dir := IsolatedHomeDir(); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".scopeforge")
	}
	return ""
}

// CacheDir returns the per-user cache root for derived/regenerable artefacts
// (MCP handshake snapshots, plugin latency telemetry). ScopeForge 使用
// <os-cache-dir>/scopeforge。
func CacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "scopeforge")
}
