package reasonix_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"scopeforge/internal/reasonix/memory"
	"scopeforge/internal/reasonix/plugin"
	"scopeforge/internal/reasonix/retrieval"
	"scopeforge/internal/reasonix/skill"
	"scopeforge/internal/reasonix/tool"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestSkillDiscoveryAndIndex(t *testing.T) {
	dir := t.TempDir()
	proj := t.TempDir()
	content := `---
name: web-audit
description: Web 漏洞审计流程
runas: inline
---

正文内容:先枚举,再利用。
`
	if err := writeFile(dir+"/web-audit.md", content); err != nil {
		t.Fatal(err)
	}

	s := skill.New(skill.Options{
		HomeDir:         dir,
		ReasonixHomeDir: dir,
		CustomPaths:     []string{dir},
		ProjectRoot:     proj,
	})
	names := s.List()
	var all []string
	var webAudit *skill.Skill
	for i := range names {
		all = append(all, names[i].Name)
		if names[i].Name == "web-audit" {
			webAudit = &names[i]
		}
	}
	joined := strings.Join(all, ",")
	if !strings.Contains(joined, "web-audit") {
		t.Errorf("custom skill not discovered: %v", all)
	}
	if webAudit == nil || !strings.Contains(webAudit.Body, "先枚举") {
		t.Errorf("web-audit body = %+v", webAudit)
	}
	// 索引块包含描述
	idx := skill.IndexBlock(names)
	if !strings.Contains(idx, "web-audit") || !strings.Contains(idx, "Web 漏洞审计流程") {
		t.Errorf("index = %q", idx)
	}
}

func TestMemorySaveAndIndex(t *testing.T) {
	dir := t.TempDir()
	st := memory.StoreFor(dir, dir)
	if st.Dir == "" {
		t.Fatal("store dir empty")
	}
	m := memory.Memory{
		Name:        "target-port",
		Title:       "目标端口",
		Description: "目标 10.0.0.5 开放 8080 端口",
		Type:        memory.TypeProject,
		Scope:       memory.FactScopeProject,
		Body:        "observation: nmap 输出显示 10.0.0.5:8080 open",
	}
	path, err := st.Save(m)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if path == "" {
		t.Fatal("empty save path")
	}
	// 索引行
	idx := st.Index()
	if !strings.Contains(idx, "target-port") {
		t.Errorf("index = %q", idx)
	}
	// 文件落盘
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("file check: %v", err)
	}
	// 全局作用域
	m2 := m
	m2.Name = "global-fact"
	m2.Scope = memory.FactScopeGlobal
	if _, err := st.Save(m2); err != nil {
		t.Errorf("global save: %v", err)
	}
}

func TestBM25ChineseRetrieval(t *testing.T) {
	// 上游 retrieval 为函数式 BM25:Tokens/Counts/BM25Score
	docs := []string{
		"目标 10.0.0.5 开放 8080 端口,运行 nginx",
		"flag 提交到平台 submit_flag 接口",
		"数据库 root 密码是 password123",
	}
	var countsList []map[string]int
	var lens []int
	for _, d := range docs {
		terms := retrieval.Tokens(d)
		countsList = append(countsList, retrieval.Counts(terms))
		lens = append(lens, len(terms))
	}
	df := retrieval.DocumentFrequency(countsList)
	total := len(docs)
	avgLen := float64(lens[0]+lens[1]+lens[2]) / float64(total)

	query := mustQueryTerms(t, "8080 端口 nginx")
	scores := make([]float64, len(docs))
	for i := range docs {
		scores[i] = retrieval.BM25Score(countsList[i], lens[i], query, df, total, avgLen)
	}
	best := 0
	for i := 1; i < len(scores); i++ {
		if scores[i] > scores[best] {
			best = i
		}
	}
	if best != 0 {
		t.Errorf("best doc = %d (score %.3f), want 0; scores=%v", best, scores[best], scores)
	}
	// 中文分词:单字切分
	terms := retrieval.Tokens("端口")
	if len(terms) != 2 || terms[0] != "端" || terms[1] != "口" {
		t.Errorf("cjk tokens = %v", terms)
	}
	// 拉丁词小写
	terms2 := retrieval.Tokens("NMAP Scan")
	if !contains(terms2, "nmap") {
		t.Errorf("latin lowercase tokens = %v", terms2)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestMCPStdioEchoServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	host, tools, err := plugin.StartAll(ctx, []plugin.Spec{{
		Name:    "echo",
		Type:    "stdio",
		Command: "node",
		Args:    []string{"testdata/echo_server.js"},
		Dir:     ".",
	}})
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	defer host.Close()

	var echoTool tool.Tool
	for _, tl := range tools {
		if tl.Name() == "mcp__echo__echo" {
			echoTool = tl
			break
		}
	}
	if echoTool == nil {
		t.Fatalf("mcp__echo__echo not injected; got %d tools", len(tools))
	}
	out, err := echoTool.Execute(ctx, []byte(`{"text":"hello mcp"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "echo: hello mcp") {
		t.Errorf("out = %q", out)
	}
}

func mustQueryTerms(t *testing.T, q string) []string {
	t.Helper()
	terms, err := retrieval.QueryTerms(q)
	if err != nil {
		t.Fatalf("query terms: %v", err)
	}
	return terms
}
