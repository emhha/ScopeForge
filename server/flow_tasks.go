package server

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/emhha/scopeforge/agent"
)

// Keep the caller's values/cancellation while also following the Engine's task
// cancellation. Admission uses the same lock and delete barrier as pause/delete.
// The operation remains in-flight until final events and status are persisted.
func (s *Server) workflowExecutionContext(parent context.Context) (context.Context, context.CancelFunc, func(), error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	taskID := agent.RunInfoFrom(parent).TaskID
	if taskID == 0 {
		return ctx, cancel, cancel, nil
	}
	key := strconv.FormatInt(taskID, 10)
	s.concMu.Lock()
	defer s.concMu.Unlock()
	task, exists := s.m.Task(key)
	if !exists || s.engine == nil || s.engine.IsDeleting(key) {
		cancel()
		return nil, nil, nil, fmt.Errorf("task is missing or being deleted")
	}
	state := task.lifecycleSnapshot()
	if state.Paused || state.Queued || s.engine.IsPaused(key) {
		cancel()
		return nil, nil, nil, fmt.Errorf("task is paused or queued")
	}
	if !s.engine.beginTaskOperation(key) {
		cancel()
		return nil, nil, nil, fmt.Errorf("task is being deleted")
	}
	stop := context.AfterFunc(s.engine.execContextFor(s.ctx, key), cancel)
	release := sync.OnceFunc(func() {
		stop()
		cancel()
		s.engine.decInflight(key)
	})
	return ctx, cancel, release, nil
}
