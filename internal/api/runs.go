package api

// POST /api/v1/runs/{id}/stop — 运行中任务控制(06.15):
// 停止任务(cancel → run_done + 容器清理)。
// 取消函数由 build.App 注册表持有(RunTask 启动时注册,run_done 后注销)。

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleRunStop(w http.ResponseWriter, r *http.Request) {
	if s.deps.Runner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "runner not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	cancel := s.deps.Runner.RunControlFor(id)
	if cancel == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务未在运行或已结束(进程重启前的任务无法控制;已中断任务可直接重新发起)"})
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "action": "stop"})
}
