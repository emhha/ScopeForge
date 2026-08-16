package api

// POST /api/v1/run 端点测试(mock Runner;异步启动逻辑由 build.App 集成覆盖)。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockRunner 记录调用。
type mockRunner struct {
	taskCalls []string
	taskErr   error
	taskCtx   context.Context
}

func (m *mockRunner) RunControlFor(id string) context.CancelFunc { return nil }

func (m *mockRunner) RunTask(ctx context.Context, task string, focusURL ...string) (string, error) {
	m.taskCalls = append(m.taskCalls, task)
	m.taskCtx = ctx
	if m.taskErr != nil {
		return "", m.taskErr
	}
	return "task-1", nil
}

func postRun(t *testing.T, srv *httptest.Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/run", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestRunTaskMode(t *testing.T) {
	runner := &mockRunner{}
	srv := httptest.NewServer(NewServer(Deps{Runner: runner}).Handler())
	defer srv.Close()

	resp, out := postRun(t, srv, `{"mode":"task","task":"http://test.com 是你的目标,找到高危漏洞"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d out=%v", resp.StatusCode, out)
	}
	if out["run_id"] != "task-1" || out["mode"] != "task" {
		t.Errorf("out=%v", out)
	}
	if len(runner.taskCalls) != 1 || !strings.Contains(runner.taskCalls[0], "test.com") {
		t.Errorf("taskCalls=%v", runner.taskCalls)
	}
	// 回归(修复:传请求 ctx 导致任务立即取消):请求已返回,ctx 必须仍存活
	if err := runner.taskCtx.Err(); err != nil {
		t.Errorf("RunTask 收到的 ctx 在请求返回后被取消(%v)——必须用独立 ctx 而非 r.Context()", err)
	}
}

func TestRunValidation(t *testing.T) {
	runner := &mockRunner{}
	srv := httptest.NewServer(NewServer(Deps{Runner: runner}).Handler())
	defer srv.Close()

	cases := []struct {
		body string
		want int
	}{
		{`{"mode":"task"}`, http.StatusBadRequest},      // 缺 task
		{`{"mode":"challenge"}`, http.StatusBadRequest}, // 缺 challenge_id
		{`{"mode":"unknown"}`, http.StatusBadRequest},   // 未知 mode
		{`not-json`, http.StatusBadRequest},             // 坏 json
	}
	for _, c := range cases {
		resp, _ := postRun(t, srv, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("body=%s status=%d want %d", c.body, resp.StatusCode, c.want)
		}
	}
	if len(runner.taskCalls) != 0 {
		t.Errorf("校验失败不应触发 Runner: %v", runner.taskCalls)
	}
}

func TestRunUnavailable(t *testing.T) {
	srv := httptest.NewServer(NewServer(Deps{}).Handler())
	defer srv.Close()
	resp, _ := postRun(t, srv, `{"mode":"task","task":"x"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}
