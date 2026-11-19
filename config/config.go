// Package config loads runtime configuration from a JSON file, with environment
// variables taking precedence. Business data defaults to a local SQLite file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Database configures SQLite. Legacy PostgreSQL fields only identify unsupported
// configurations so startup never silently creates an empty replacement database.
type Database struct {
	Path     string `json:"path,omitempty"`
	Driver   string `json:"driver,omitempty"`
	DSN      string `json:"dsn"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
}

// Config is the on-disk config file shape.
type Config struct {
	Database Database `json:"database"`
	SkillDir string   `json:"skill_dir"`
}

// BaseDir is the directory that anchors all runtime artifacts (config.json and
// the data/ store). It is the directory the running binary lives in, so a
// distributed executable keeps its files next to itself on any OS (Windows,
// Linux, …) regardless of the working directory it is launched from.
//
// When launched via `go run`, the binary is throwaway: it sits either in a temp
// build dir (cache miss → fresh link) OR straight inside the Go build cache
// (cache hit → run from $GOCACHE/.../...-d). We detect both and fall back to the
// current working directory so dev artifacts (data/) and config
// resolve against the project dir, not the throwaway binary's location.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if isGoRunDir(dir) {
		return "." // throwaway `go run` binary → use CWD
	}
	return dir
}

// isGoRunDir reports whether dir is where `go run` parked its executable: under
// the system temp dir (cache miss), or anywhere inside a Go build cache
// (…/go-build/…, the cache-hit case — NOT under os.TempDir(), which is why the
// old temp-only check failed intermittently). In both cases the binary is
// throwaway, so config/data must resolve against the CWD.
func isGoRunDir(dir string) bool {
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "go-build" {
			return true
		}
	}
	return false
}

// Path returns the config file path. Resolution order:
//  1. env ARTEX_CONFIG (explicit override)
//  2. ./config.json in the current working directory (running from the project
//     dir — robust no matter where `go run` placed the temp/cached binary)
//  3. config.json next to the executable (a distributed binary keeps it beside)
//
// The first existing file wins. If none exist, the CWD path is returned so the
// "not found" message points at the project dir the user most likely expected.
func Path() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_CONFIG")); v != "" {
		return v
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "config.json"))
	}
	candidates = append(candidates, filepath.Join(BaseDir(), "config.json"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// Load reads and parses the config file. A missing/unreadable file yields a zero
// Config (so callers fall back to defaults) rather than an error.
func Load() Config {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// SkillDir returns the skill root directory with precedence:
//
//	env ARTEX_SKILL_DIR  >  config file (skill_dir)  >  BaseDir()/skills
//
// The directory is created if it does not exist.
func SkillDir() string {
	var d string
	if v := strings.TrimSpace(os.Getenv("ARTEX_SKILL_DIR")); v != "" {
		d = v
	} else if v := strings.TrimSpace(Load().SkillDir); v != "" {
		d = v
	} else {
		d = filepath.Join(BaseDir(), "skills")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// SQLitePath resolves a local database file. Default storage follows the data
// directory; relative configured paths are relative to the config file.
func SQLitePath(dataDir string) (path, source string, err error) {
	if value := strings.TrimSpace(os.Getenv("ARTEX_DB_PATH")); value != "" {
		return value, "环境变量 ARTEX_DB_PATH", nil
	}
	var loaded Config
	raw, readErr := os.ReadFile(Path())
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", "", fmt.Errorf("read database config: %w", readErr)
	}
	if readErr == nil {
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return "", "", fmt.Errorf("invalid config.json: %w", err)
		}
	}
	database := loaded.Database
	if database.Driver != "" && database.Driver != "sqlite" {
		return "", "", fmt.Errorf("database.driver must be sqlite")
	}

	if database.Path != "" {
		path = database.Path
		if !filepath.IsAbs(path) && !strings.HasPrefix(path, "file:") {
			path = filepath.Join(filepath.Dir(Path()), path)
		}
		return path, "配置文件 " + Path() + " (database.path)", nil
	}

	if database.DSN != "" || database.Host != "" || database.DBName != "" || database.User != "" || os.Getenv("ARTEX_PG_DSN") != "" {
		return "", "", fmt.Errorf("不支持 PostgreSQL 配置；请使用 SQLite，并通过 database.path 或 ARTEX_DB_PATH 指定数据库文件")
	}
	if dataDir == "" {
		dataDir = filepath.Join(BaseDir(), "data")
	}
	return filepath.Join(dataDir, "artex.sqlite"), "默认本地 SQLite", nil
}
