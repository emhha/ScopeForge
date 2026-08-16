package api

// POST /api/v1/run — Web 发起任务(M3.5,serve 内异步 run)。
// 两种模式:
//
//	{"mode":"task","task":"http://test.com 是你的目标...","platform_url":"..."}
//	{"mode":"challenge","challenge_id":"xxx","observer":null}  // null = 跟随配置(默认启用)
//
// 返回 {"run_id":"...","mode":"..."};执行进度经事件流(run_started/run_done)。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Runner 是任务启动器(由 build.App 实现;接口隔离避免 api→build 依赖)。
type Runner interface {
	RunTask(ctx context.Context, task string, focusURL ...string) (string, error)
	// RunControlFor 返回运行中任务的取消函数(06.15;nil = 未运行/已结束)。
	RunControlFor(id string) context.CancelFunc
}

type runRequest struct {
	Mode        string `json:"mode"` // task(唯一模式)
	Task        string `json:"task"`
	PlatformURL string `json:"platform_url"`
}

type runResponse struct {
	RunID string `json:"run_id"`
	Mode  string `json:"mode"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.Runner == nil {
		http.Error(w, "run: 服务未装配 Runner", http.StatusServiceUnavailable)
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("run: bad json: %v", err), http.StatusBadRequest)
		return
	}
	switch req.Mode {
	case "task":
		if req.Task == "" {
			http.Error(w, "run: task 字段不能为空", http.StatusBadRequest)
			return
		}
		runID, err := s.deps.Runner.RunTask(context.Background(), req.Task, req.PlatformURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusAccepted, runResponse{RunID: runID, Mode: "task"})
	default:
		http.Error(w, fmt.Sprintf("run: 未知 mode %q(仅支持 task)", req.Mode), http.StatusBadRequest)
	}
}
