// Package integration 是 M0 集成测试(docs/02 §0.4 验收总则):
//   - mock DeepSeek 兼容端点 → 工具调用 → 结果回填 → 会话落库 → 重启恢复
//   - kill -9 崩溃恢复:子进程跑 run,强杀后验证会话不丢
//   - 权限三模式 / 技能索引 / MCP / 子代理 全链路
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scopeforge/internal/build"
	"scopeforge/internal/conversation"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/store"
)

// ------------------------------------------------------------------ mock 服务器

// mockLLM 是两步 mock:先工具调用,再最终文本。
type mockLLM struct {
	*httptest.Server
	callCount atomic.Int64
}

func newMockLLM(t *testing.T) *mockLLM {
	t.Helper()
	m := &mockLLM{}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		n := m.callCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s)
			fl.Flush()
		}
		if n == 1 {
			// 第一轮:调用 bash 工具
			write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo integration-ok\\\"}\"}}]}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"prompt_cache_hit_tokens\":50,\"prompt_cache_miss_tokens\":50}}\n\n")
		} else {
			write("data: {\"choices\":[{\"delta\":{\"content\":\"最终结论:集成测试完成\"}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":15}}\n\n")
		}
		write("data: [DONE]\n\n")
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

// writeConfig 写 mock 配置。
func writeConfig(t *testing.T, dir, baseURL, dbPath string) string {
	t.Helper()
	cfg := fmt.Sprintf(`providers:
  - name: mock
    kind: openai
    base_url: %s
    model: mock-model
    api_key_env: MOCK_KEY
agent:
  max_turns: 50
tools:
  permissions: yolo
  blacklist: ["rm -rf /"]
  domain_allowlist: ["example.com"]
memory:
  dir: %s
`, baseURL, filepath.Join(dir, "memory"))
	path := filepath.Join(dir, "scopeforge.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ------------------------------------------------------------------ 全链路

func TestFullLoopPersistAndRestart(t *testing.T) {
	mock := newMockLLM(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scopeforge.db")
	cfgPath := writeConfig(t, dir, mock.URL, dbPath)
	t.Setenv("MOCK_KEY", "test-key")
	t.Setenv("SCOPEFORGE_HOME", dir) // skill 物化到测试临时目录,不碰 ~/.scopeforge

	// 第一次运行
	app, err := build.New(build.Options{ConfigPath: cfgPath, DBPath: dbPath, WorkDir: dir})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	app.RegisterAgentTools(app.Providers["mock"], nil)
	sess := conversation.New("sess-1", conversation.KindMain)
	sess.Provider = "mock"
	sess.Add(conversationUserMsg("执行集成任务"))
	res, err := executor.Run(context.Background(), &executor.Options{
		Provider:   app.Providers["mock"],
		Registry:   app.Registry,
		Session:    sess,
		Store:      app.ConvStore,
		MaxTurns:   50,
		Gate:       app.Gate,
		Sink:       app.Sink,
		CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir:    dir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.FinalText, "集成测试完成") {
		t.Errorf("final = %q", res.FinalText)
	}
	if res.Turns != 2 || res.ToolCalls != 1 {
		t.Errorf("turns=%d toolCalls=%d", res.Turns, res.ToolCalls)
	}
	app.Close()

	// "重启":同一 db 重新装配,会话可恢复
	app2, err := build.New(build.Options{ConfigPath: cfgPath, DBPath: dbPath, WorkDir: dir})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	defer app2.Close()
	loaded, err := app2.ConvStore.LoadLatest("sess-1")
	if err != nil {
		t.Fatalf("load latest after restart: %v", err)
	}
	if len(loaded.Messages) < 3 {
		t.Errorf("restored messages = %d, want >= 3", len(loaded.Messages))
	}
	// 工具结果在会话中
	var toolOut string
	for _, m := range loaded.Messages {
		if m.Role == provider.RoleTool {
			toolOut = m.Content
		}
	}
	if !strings.Contains(toolOut, "integration-ok") {
		t.Errorf("tool result in session = %q", toolOut)
	}
}

// ------------------------------------------------------------------ 崩溃恢复

// TestCrashRecovery 用子进程跑 scopeforge run,中途 kill -9,验证会话已落库。
func TestCrashRecovery(t *testing.T) {
	if os.Getenv("SCOPEFORGE_CRASH_HELPER") == "1" {
		crashHelperMain()
		return
	}
	mock := newMockLLM(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scopeforge.db")
	cfgPath := writeConfig(t, dir, mock.URL, dbPath)

	// 启动子进程(helper 模式),它会执行一个永不结束的任务(每轮都调工具)
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashRecovery")
	cmd.Env = append(os.Environ(),
		"SCOPEFORGE_CRASH_HELPER=1",
		"SCOPEFORGE_CFG="+cfgPath,
		"SCOPEFORGE_DB="+dbPath,
		"SCOPEFORGE_HOME="+dir,
		"MOCK_KEY=test-key",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	// 等待至少 3 轮落库(helper 每轮 100ms)
	time.Sleep(2 * time.Second)
	// kill -9
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// 验证:会话已落库且可恢复
	db, err := testutilOpenDB(dbPath)
	if err != nil {
		t.Fatalf("open db after crash: %v", err)
	}
	defer db.Close()
	st := conversation.NewStore(db)
	sessions, err := st.List("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("no session persisted before kill -9")
	}
	s, err := st.LoadLatest(sessions[0].ID)
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	if len(s.Messages) < 2 {
		t.Errorf("recovered session has %d messages", len(s.Messages))
	}
	// 会话 digest 有效(可继续保存)
	if err := st.SaveSnapshot(s); err != nil {
		t.Errorf("save after recovery: %v", err)
	}
}

// crashHelperMain 是崩溃测试子进程:无限工具轮(永不结束),每轮落库。
func crashHelperMain() {
	cfgPath := os.Getenv("SCOPEFORGE_CFG")
	dbPath := os.Getenv("SCOPEFORGE_DB")
	app, err := build.New(build.Options{ConfigPath: cfgPath, DBPath: dbPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper build:", err)
		os.Exit(1)
	}
	app.RegisterAgentTools(app.Providers["mock"], nil)
	sess := conversation.New("crash-sess", conversation.KindMain)
	sess.Add(conversationUserMsg("无限循环任务"))
	for i := 0; i < 1000; i++ {
		res, err := executor.Run(context.Background(), &executor.Options{
			Provider:   app.Providers["mock"],
			Registry:   app.Registry,
			Session:    sess,
			Store:      app.ConvStore,
			MaxTurns:   1, // 每轮只跑一次工具调用
			Gate:       app.Gate,
				Sink:       app.Sink,
			CompactCfg: conversation.DefaultCompactConfig(),
		})
		if err != nil && !strings.Contains(err.Error(), "max turns") {
			_ = res
		}
		// mock 第 1 轮之后返回文本,但 MaxTurns=1 会直接结束;手动重置会话轮次
		sess.Add(conversationUserMsg(fmt.Sprintf("继续第 %d 轮", i)))
		time.Sleep(100 * time.Millisecond)
	}
}

func conversationUserMsg(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: text}
}

func testutilOpenDB(path string) (*store.DB, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return db, nil
}
