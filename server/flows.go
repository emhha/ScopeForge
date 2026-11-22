package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowcli"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func (s *Server) registerFlowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/flows/validate", s.validateWorkflow)
	mux.HandleFunc("GET /api/flows/{id}/versions", s.workflowVersions)
	mux.HandleFunc("POST /api/flows/{id}/unpublish", s.unpublishWorkflow)
	mux.HandleFunc("POST /api/flows/{id}/rollback", s.rollbackWorkflow)
	mux.HandleFunc("POST /api/flow-runs", s.inlineWorkflow)
	mux.HandleFunc("GET /api/flow-runs", s.allWorkflowRuns)
	mux.HandleFunc("POST /api/flow-runs/{run}/save-as", s.saveRunAsWorkflow)
	mux.HandleFunc("GET /api/flow-artifacts/{artifact}", s.getFlowArtifact)
	mux.HandleFunc("GET /api/flow-runs/{run}/artifacts", s.listFlowArtifacts)
	mux.HandleFunc("GET /api/flows", s.listFlows)
	mux.HandleFunc("GET /api/flows/catalog", s.flowCatalog)
	mux.HandleFunc("POST /api/flows", s.saveFlow)
	mux.HandleFunc("PUT /api/flows/{id}", s.saveFlow)
	mux.HandleFunc("POST /api/flows/{id}/publish", s.publishFlow)
	mux.HandleFunc("POST /api/flows/{id}/run", s.startFlow)
	mux.HandleFunc("GET /api/flows/{id}/runs", s.listFlowRuns)
	mux.HandleFunc("GET /api/flow-runs/{run}", s.getFlowRun)
	mux.HandleFunc("GET /api/flow-runs/{run}/events", s.streamFlowEvents)
	mux.HandleFunc("POST /api/flow-runs/{run}/cancel", s.cancelFlow)
}
func flowInspect() actool.CallableTool {
	return toolkit.New("inspect", "示例：检查字符串是否以 https:// 开头，不发送网络请求", map[string]any{"type": "object", "required": []string{"target"}, "properties": map[string]any{"target": map[string]any{"type": "string"}}}, func(_ context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return toolkit.Output{}, err
		}
		if strings.HasPrefix(in.Target, "https://") {
			return toolkit.Text("allowed"), nil
		}
		return toolkit.Text("denied"), nil
	})
}
func (s *Server) flowPool() map[string]actool.CallableTool {
	out := map[string]actool.CallableTool{"inspect": flowInspect(), "workflow_command": s.commandWorkflowTool(""), "Bash": s.bashWorkflowTool()}
	for _, item := range s.networkTools() {
		out[item.Declaration().Name] = item
	}
	for _, item := range flowcli.Tools(s.executeWorkflowHost) {
		out[item.Declaration().Name] = item
	}
	for name, item := range buildDomainReg(s.m.assets) {
		out[name] = item
	}
	custom, _ := s.m.pg.ListCustomTools()
	for _, row := range custom {
		if !row.Enabled {
			continue
		}
		if row.Kind == "shell" {
			out[row.Key] = s.commandWorkflowTool(row.Key)
		} else {
			out[row.Key] = s.buildCustomTool(row)
		}
	}
	return out
}
func (s *Server) flowCatalog(w http.ResponseWriter, r *http.Request) {
	pool, close, err := s.flowToolPool(r.Context(), r.URL.Query().Get("agent"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer close()
	writeJSON(w, 200, map[string]any{"tools": sortedFlowTools(pool), "example": flowproto.ExampleDefinition(), "node_types": []string{"start", "tool", "transform", "branch", "parallel", "join", "end"}, "operations": flowproto.Operations, "definition_schema": flowproto.DefinitionSchema()})
}
func (s *Server) listFlows(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.ListFlows()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) saveFlow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Definition flowproto.Definition `json:"definition"`
		Revision   int                  `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if r.Method == http.MethodPut && id <= 0 {
		writeErr(w, 400, "invalid flow id")
		return
	}
	if id == 0 && in.Revision != 0 {
		writeErr(w, 400, "new flow revision must be zero")
		return
	}
	if id > 0 {
		old, err := s.m.pg.GetFlow(id)
		if err != nil {
			writeErr(w, 404, "flow not found")
			return
		}
		if old.Name != in.Definition.Name {
			versions, _ := s.m.pg.FlowVersions(id)
			if len(versions) > 0 {
				writeErr(w, 400, "已发布工作流的工具名称固定，请复制为新流程后改名")
				return
			}
		}
	}
	in.Definition.Version = in.Revision + 1
	pool, close, err := s.flowToolPool(r.Context(), "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer close()
	if _, err := flowproto.Compile(in.Definition, pool); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	raw, _ := json.Marshal(in.Definition)
	saved, err := s.m.pg.SaveFlow(id, in.Definition.Name, raw, in.Revision)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	row, err := s.m.pg.GetFlow(saved)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, row)
}
func (s *Server) publishFlow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	row, err := s.m.pg.GetFlow(id)
	if err != nil {
		writeErr(w, 404, "flow not found")
		return
	}
	var in struct {
		Revision int      `json:"revision"`
		Agents   []string `json:"agents"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Revision != row.Revision {
		writeErr(w, 409, db.ErrFlowConflict.Error())
		return
	}
	var candidate flowproto.Definition
	_ = json.Unmarshal(row.Definition, &candidate)
	if err := s.checkWorkflowCycles(id, candidate, map[int64]bool{}); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, key := range in.Agents {
		a, err := s.m.pg.GetAgentByKey(key)
		if err != nil || a == nil {
			writeErr(w, 400, "unknown agent: "+key)
			return
		}
		pool, close, err := s.flowToolPool(r.Context(), key)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		var def flowproto.Definition
		_ = json.Unmarshal(row.Definition, &def)
		_, compileErr := flowproto.Compile(def, pool)
		close()
		if compileErr != nil {
			writeErr(w, 400, fmt.Sprintf("Agent %s 无法执行该工作流: %v", key, compileErr))
			return
		}
	}
	if _, err := s.m.pg.RegisterFlow(id, in.Revision, in.Agents); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	updated, err := s.m.pg.GetFlow(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, updated)
}
func (s *Server) startFlow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	row, err := s.m.pg.GetFlow(id)
	if err != nil {
		writeErr(w, 404, "flow not found")
		return
	}
	var in struct {
		Input  json.RawMessage `json:"input"`
		TaskID int64           `json:"task_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var def flowproto.Definition
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if in.TaskID < 0 {
		writeErr(w, 400, "invalid task id")
		return
	}
	if in.TaskID > 0 {
		task, err := s.m.pg.GetTask(in.TaskID)
		if err != nil || task == nil {
			writeErr(w, 400, "task not found")
			return
		}
	}
	ctx := agent.WithRunInfo(s.ctx, agent.RunInfo{TaskID: in.TaskID})
	runID, reader, finish, err := s.beginWorkflow(ctx, id, 0, def, in.Input, "manual", "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	go func() { result, err := s.pipeWorkflow(ctx, runID, reader, nil); finish(result, err) }()
	writeJSON(w, 202, map[string]any{"id": runID, "status": "running"})
}

func (s *Server) flowRun(id string) (*db.FlowRun, error) {
	run, err := s.m.pg.GetFlowRun(id)
	if err != nil {
		return nil, err
	}
	if run.Status == "running" {
		if _, active := s.flowCancels.Load(id); !active {
			_ = s.m.pg.FinishFlowRun(id, "interrupted", "", "服务重启后，试运行已中断")
			return s.m.pg.GetFlowRun(id)
		}
	}
	return run, nil
}
func (s *Server) getFlowRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.flowRun(r.PathValue("run"))
	if err != nil {
		writeErr(w, 404, "run not found")
		return
	}
	writeJSON(w, 200, run)
}
func (s *Server) cancelFlow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	if _, err := s.flowRun(id); err != nil {
		writeErr(w, 404, "run not found")
		return
	}
	if cancel, ok := s.flowCancels.Load(id); ok {
		cancel.(context.CancelFunc)()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) streamFlowEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	if _, err := s.flowRun(id); err != nil {
		writeErr(w, 404, "run not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		since, _ = strconv.ParseInt(last, 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Read status first: a terminal status guarantees all its events were
		// persisted before the following query. Reading it after events could
		// close the stream while the last node events are still unread.
		run, err := s.flowRun(id)
		if err != nil {
			return
		}
		children, err := s.m.pg.RunningFlowChildren(id)
		if err != nil {
			return
		}
		pendingChild := false
		for _, child := range children {
			row, err := s.flowRun(child)
			if err != nil {
				return
			}
			if row.Status == "running" {
				pendingChild = true
			}
		}
		events, err := s.m.pg.FlowTreeEvents(id, since)
		if err != nil {
			return
		}
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, data)
			since = ev.ID
		}

		if run.Status != "running" && !pendingChild && len(events) < 500 {
			data, _ := json.Marshal(run)
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", data)
			flusher.Flush()
			return
		}
		fmt.Fprint(w, ": tick\n\n")
		flusher.Flush()
		if len(events) == 500 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// Only explicitly bound published versions become Agent tools. Dependencies are
// resolved from the caller's permitted tool set; drafts and recursive flows are excluded.
func (s *Server) flowTools(ctx context.Context, key string, pool []actool.CallableTool) []actool.CallableTool {
	rows, err := s.m.pg.ListCustomTools()
	if err != nil {
		return nil
	}
	out := []actool.CallableTool{}
	for _, row := range rows {
		if row.Kind == "workflow" && row.Enabled && contains(row.Agents, key) {
			out = append(out, &nativeWorkflowTool{s: s, row: row, key: key})
		}
	}
	for _, item := range s.workflowControlTools(key) {
		row, err := s.m.pg.GetTool(item.Declaration().Name)
		if err == nil && row != nil && row.Enabled && contains(row.Agents, key) {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) listFlowRuns(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.m.pg.GetFlow(id); err != nil {
		writeErr(w, 404, "flow not found")
		return
	}
	runs, err := s.m.pg.ListFlowRuns(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, runs)
}
