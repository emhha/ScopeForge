// Package builtinskills 承载 ScopeForge 内置技能(靶场接入卡/漏洞分级/报告等)。
//
// 06.14 净化:技能从 reasonix/skill/builtincontent 移出到自有包,
// reasonix 恢复上游原貌(可机械同步)。运行时"首次启动 materialize"到
// config.skills.dir 指向的磁盘目录 —— 用户可直接编辑,重启不覆盖。
package builtinskills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills
var skillsFS embed.FS

// Item 是技能文件(SKILL.md + 附属文件)。
type Item struct {
	Name string // 技能名(目录名)
	Path string // embed 内路径(skills/<name>/SKILL.md)
	Body string
}

// All 返回内置技能列表(embed 只读,供 Materialize 与测试)。
func All() ([]Item, error) {
	dirs, err := fs.ReadDir(skillsFS, "skills")
	if err != nil {
		return nil, fmt.Errorf("builtinskills: read skills dir: %w", err)
	}
	out := make([]Item, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		body, err := skillsFS.ReadFile(filepath.ToSlash(filepath.Join("skills", d.Name(), "SKILL.md")))
		if err != nil {
			// 辅助资源目录(如 templates/)无 SKILL.md,不是技能,跳过
			continue
		}
		out = append(out, Item{Name: d.Name(), Path: filepath.Join("skills", d.Name(), "SKILL.md"), Body: string(body)})
	}
	return out, nil
}

// Materialize 把内置技能写盘到 dir(仅当目标 SKILL.md 不存在时写入)。
// 用户修改过的技能文件不会被覆盖;返回写入数。
func Materialize(dir string) (int, error) {
	items, err := All()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		target := filepath.Join(dir, it.Name, "SKILL.md")
		if _, err := os.Stat(target); err == nil {
			continue // 已存在(用户可能改过),不覆盖
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, fmt.Errorf("builtinskills: mkdir %s: %w", filepath.Dir(target), err)
		}
		// 附属文件(模板等)一并落盘:embed 内同目录全部文件
		sub, err := fs.ReadDir(skillsFS, filepath.ToSlash(filepath.Join("skills", it.Name)))
		if err != nil {
			return n, err
		}
		for _, f := range sub {
			if f.IsDir() {
				continue
			}
			data, err := skillsFS.ReadFile(filepath.ToSlash(filepath.Join("skills", it.Name, f.Name())))
			if err != nil {
				return n, err
			}
			dst := filepath.Join(dir, it.Name, f.Name())
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return n, err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return n, err
			}
			if strings.HasSuffix(f.Name(), "SKILL.md") {
				n++
			}
		}
	}
	return n, nil
}
