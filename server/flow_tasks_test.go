package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowproto"
)

func TestWorkflowPauseCancelsLinkedManualRun(t *testing.T) {
	s, do := workflowTestServer(t)
	task, err := s.m.CreateTask("review pause", "review", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	def := flowproto.Definition{Name: "review_wait", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "wait"}, {ID: "wait", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "sh", "argv": []any{"-c", "sleep 30"}}, Next: "end"}, {ID: "end", Type: "end", Output: "done"}}}
	raw, _ := json.Marshal(def)
	flowID, err := s.m.pg.SaveFlow(0, def.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := do("POST", fmt.Sprintf("/api/flows/%d/run", flowID), map[string]any{"input": map[string]any{}, "task_id": taskID})
	if rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	var started struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &started)
	cancelValue, _ := s.flowCancels.Load(started.ID)
	defer func() {
		cancelValue.(context.CancelFunc)()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, active := s.flowCancels.Load(started.ID); !active {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	runCtx, _ := s.flowContexts.Load(started.ID)
	if _, err = s.applyTaskControl(task, "pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runCtx.(context.Context).Done():
	case <-time.After(3 * time.Second):
		t.Errorf("linked manual workflow %s remains live after task %s paused", started.ID, task.ID)
	}
	drainCtx, cancelDrain := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelDrain()
	if err := s.waitTaskQuiescent(drainCtx, task.ID); err != nil {
		t.Fatal(err)
	}
	run, err := s.m.pg.GetFlowRun(started.ID)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel was not persisted before drain: %+v %v", run, err)
	}
	// A paused task cannot admit another workflow after its current run drains.
	ctx := agent.WithRunInfo(s.ctx, agent.RunInfo{TaskID: taskID})
	if _, _, _, err := s.beginWorkflow(ctx, flowID, 0, def, json.RawMessage(`{}`), "manual", ""); err == nil {
		t.Fatal("paused task admitted workflow")
	}
}

func TestWorkflowDeleteCancelsAndDrainsLinkedManualRun(t *testing.T) {
	s, do := workflowTestServer(t)
	task, err := s.m.CreateTask("delete active flow", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	def := flowproto.Definition{Name: "delete_active", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{
		{ID: "begin", Type: "start", Next: "wait"},
		{ID: "wait", Type: "tool", Tool: "Bash", Arguments: map[string]any{"command": "sleep 30"}, Next: "end"},
		{ID: "end", Type: "end", Output: "unexpected"},
	}}
	raw, _ := json.Marshal(def)
	flowID, err := s.m.pg.SaveFlow(0, def.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.RegisterFlow(flowID, 1, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	response := do("POST", fmt.Sprintf("/api/flows/%d/run", flowID), map[string]any{"input": map[string]any{}, "task_id": taskID})
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	var started struct{ ID string }
	if err = json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if s.engine.inflightCount(task.ID) == 0 {
		t.Fatal("workflow wasn't included in task drain")
	}
	response = do("DELETE", "/api/tasks/"+task.ID, map[string]any{"delete_files": true})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if _, active := s.flowCancels.Load(started.ID); active {
		t.Fatal("delete returned before workflow drained")
	}
	if _, err = s.m.pg.GetFlowRun(started.ID); err == nil {
		t.Fatal("run survived task deletion")
	}
	if _, err = s.m.pg.GetFlow(flowID); err != nil {
		t.Fatal("deletion removed reusable workflow")
	}
	if published, err := s.m.pg.GetTool("flow_" + def.Name); err != nil || published == nil {
		t.Fatal("deletion removed publication")
	}
	if err = s.m.pg.CreateWorkflowRun(db.FlowRun{ID: "late-writer", TaskID: taskID, Definition: raw, Input: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("late writer recreated data for deleted task")
	}
}

func TestWorkflowRestartResolvesAbandonedRunsBeforeDeletion(t *testing.T) {
	s, _ := workflowTestServer(t)
	task, err := s.m.CreateTask("abandoned workflow", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	// More than a list page: recovery must not depend on visiting the UI.
	for i := 0; i < 105; i++ {
		if err := s.m.pg.CreateWorkflowRun(db.FlowRun{ID: fmt.Sprintf("abandoned-%d", i), TaskID: taskID, Definition: json.RawMessage(`{}`), Input: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	New(ctx, s.m, t.TempDir(), s.m.dir, t.TempDir())
	var running int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM flow_runs WHERE status='running'`).Scan(&running); err != nil || running != 0 {
		t.Fatalf("abandoned runs: %d %v", running, err)
	}
	if _, err = s.m.DeleteTask(task.ID, DeleteTaskOptions{DeleteFiles: true}); err != nil {
		t.Fatal(err)
	}
}
func TestWorkflowDeleteTaskRemovesEvidence(t *testing.T) {
	s, _ := workflowTestServer(t)
	task, err := s.m.CreateTask("review delete", "review", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	runID := "review-evidence"
	err = s.m.pg.CreateWorkflowRun(db.FlowRun{ID: runID, TaskID: taskID, Source: "manual", Definition: json.RawMessage(`{}`), Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := (flowArtifacts{s: s, run: runID}).Put(t.Context(), []byte("review evidence"), "text/plain", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.FinishFlowRun(runID, "completed", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.DeleteTask(task.ID, DeleteTaskOptions{DeleteFiles: true, DeleteLLMRecords: true}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM flow_runs WHERE task_id=?1`, taskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("deleted task retains %d workflow run(s)", count)
	}
	if _, err = os.Stat(filepath.Join(s.m.dir, "flow-artifacts", artifact.ID)); err == nil {
		t.Errorf("deleted task retains workflow artifact file")
	}
}

func seedTaskWorkflowEvidence(t *testing.T, s *Server, id string, taskID int64, parent string) string {
	t.Helper()
	if err := s.m.pg.CreateWorkflowRun(db.FlowRun{ID: id, TaskID: taskID, ParentRunID: parent, Source: "manual", Definition: json.RawMessage(`{}`), Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	artifact, err := (flowArtifacts{s: s, run: id}).Put(t.Context(), []byte(id), "text/plain", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.AppendFlowPayload(id, "graph.node.complete", "fixture", "", json.RawMessage(`{"text":"evidence"}`)); err != nil {
		t.Fatal(err)
	}
	return artifact.ID
}

func TestWorkflowEvidenceDeletionIsTransactionalAndIsolated(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		for _, deleteFiles := range []bool{false, true} {
			t.Run(fmt.Sprintf("rollback=%v/files=%v", rollback, deleteFiles), func(t *testing.T) {
				s, _ := workflowTestServer(t)
				task, err := s.m.CreateTask("delete evidence", "review", nil, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				other, err := s.m.CreateTask("other task", "review", nil, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				taskID, _ := strconv.ParseInt(task.ID, 10, 64)
				otherID, _ := strconv.ParseInt(other.ID, 10, 64)
				files := map[string]string{}
				files["root"] = seedTaskWorkflowEvidence(t, s, "root", taskID, "")
				// Legacy child ownership can be missing; descend through the call tree.
				files["child"] = seedTaskWorkflowEvidence(t, s, "child", 0, "root")
				files["other"] = seedTaskWorkflowEvidence(t, s, "other", otherID, "")
				files["standalone"] = seedTaskWorkflowEvidence(t, s, "standalone", 0, "")
				for id := range files {
					if err := s.m.pg.FinishFlowRun(id, "completed", "done", ""); err != nil {
						t.Fatal(err)
					}
				}
				if rollback {
					if _, err := s.m.pg.Exec(fmt.Sprintf(`CREATE TRIGGER fail_workflow_task_delete BEFORE DELETE ON tasks WHEN OLD.id=%d BEGIN SELECT RAISE(ABORT,'fixture rollback'); END`, taskID)); err != nil {
						t.Fatal(err)
					}
				}
				_, err = s.m.DeleteTask(task.ID, DeleteTaskOptions{DeleteFiles: deleteFiles})
				if (err != nil) != rollback {
					t.Fatalf("delete result: %v", err)
				}
				for id, artifact := range files {
					owned := id == "root" || id == "child"
					_, err := s.m.pg.GetFlowRun(id)
					wantRecord := !owned || rollback
					if (err == nil) != wantRecord {
						t.Errorf("run %s retained=%v want=%v", id, err == nil, wantRecord)
					}
					_, err = os.Stat(filepath.Join(s.m.dir, "flow-artifacts", artifact))
					wantFile := !owned || rollback || !deleteFiles
					if (err == nil) != wantFile {
						t.Errorf("artifact %s retained=%v want=%v err=%v", id, err == nil, wantFile, err)
					}
					var events, artifacts int
					s.m.pg.QueryRow(`SELECT count(*) FROM flow_run_events WHERE run_id=?1`, id).Scan(&events)
					s.m.pg.QueryRow(`SELECT count(*) FROM flow_artifacts WHERE run_id=?1`, id).Scan(&artifacts)
					if (events > 0) != wantRecord || (artifacts > 0) != wantRecord {
						t.Errorf("orphaned/missing evidence for %s: events=%d artifacts=%d", id, events, artifacts)
					}
				}
			})
		}
	}
}

func TestWorkflowTaskAdmissionHonorsDeleteAndCallerCancellation(t *testing.T) {
	s, _ := workflowTestServer(t)
	task, err := s.m.CreateTask("barrier", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	parent, cancelParent := context.WithCancel(agent.WithRunInfo(s.ctx, agent.RunInfo{TaskID: id, SessionID: "caller-session"}))
	ctx, _, release, err := s.workflowExecutionContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	if agent.RunInfoFrom(ctx).SessionID != "caller-session" {
		t.Fatal("caller context was replaced")
	}
	cancelParent()
	if ctx.Err() == nil {
		t.Fatal("caller cancellation was lost")
	}
	release()
	if !s.beginTaskDelete(task.ID) {
		t.Fatal("could not install delete barrier")
	}
	defer s.abortTaskDelete(task.ID)
	if _, _, _, err := s.workflowExecutionContext(agent.WithRunInfo(s.ctx, agent.RunInfo{TaskID: id})); err == nil {
		t.Fatal("delete barrier admitted workflow")
	}
	if s.engine.inflightCount(task.ID) != 0 {
		t.Fatal("failed admission leaked in-flight count")
	}
	// A task pause must leave standalone workflows unaffected.
	standalone, _, releaseStandalone, err := s.workflowExecutionContext(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseStandalone()
	s.engine.Pause(task.ID, agent.AbortPausedByUser)
	if standalone.Err() != nil {
		t.Fatal("unrelated workflow was canceled")
	}
}
