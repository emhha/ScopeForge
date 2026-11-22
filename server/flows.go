package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowcli"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/graph"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func (s *Server) registerFlowRoutes(mux *http.ServeMux) {
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
	out := map[string]actool.CallableTool{"inspect": flowInspect()}
	for _, item := range s.networkTools() {
		out[item.Declaration().Name] = item
	}
	for _, item := range flowcli.Tools(agent.ExecuteCommand) {
		out[item.Declaration().Name] = item
	}
	return out
}
func (s *Server) flowCatalog(w http.ResponseWriter, r *http.Request) {
	tools := []*actool.Declaration{}
	pool := s.flowPool()
	for _, name := range []string{"inspect", "http_request", "search_mapping", "list_recon_sources", "subfinder", "amass", "merge_subdomains", "httpx", "ehole"} {
		if item := pool[name]; item != nil {
			tools = append(tools, item.Declaration())
		}
	}
	writeJSON(w, 200, map[string]any{"tools": tools, "example": flowproto.ExampleDefinition(), "node_types": []string{"start", "tool", "branch", "end"}})
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
	in.Definition.Version = in.Revision + 1
	if _, err := flowproto.Compile(in.Definition, s.flowPool()); err != nil {
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
	for _, key := range in.Agents {
		a, err := s.m.pg.GetAgentByKey(key)
		if err != nil || a == nil {
			writeErr(w, 400, "unknown agent: "+key)
			return
		}
	}
	if err := s.m.pg.PublishFlow(id, in.Revision, row.Definition, in.Agents); err != nil {
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
	compiled, err := flowproto.Compile(def, s.flowPool())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := toolkit.Validate(compiled.Tool.Declaration().InputSchema, in.Input); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.TaskID < 0 {
		writeErr(w, 400, "invalid task id")
		return
	}
	if in.TaskID > 0 {
		if _, err := s.m.pg.GetTask(in.TaskID); err != nil {
			writeErr(w, 400, "task not found")
			return
		}
	}
	runID := uuid.NewString()
	ctx, cancel := context.WithTimeout(agent.WithRunInfo(s.ctx, agent.RunInfo{TaskID: in.TaskID}), 5*time.Minute)
	s.flowCancels.Store(runID, cancel)
	if err := s.m.pg.CreateFlowRun(runID, id, row.Definition, in.Input); err != nil {
		s.flowCancels.Delete(runID)
		cancel()
		writeErr(w, 500, err.Error())
		return
	}
	go s.executeFlow(ctx, cancel, runID, compiled, in.Input)
	writeJSON(w, 202, map[string]any{"id": runID, "status": "running"})
}
func (s *Server) executeFlow(ctx context.Context, cancel context.CancelFunc, id string, compiled *flowproto.Compiled, input []byte) {
	defer cancel()
	defer s.flowCancels.Delete(id)
	result, err := s.consumeFlow(ctx, id, compiled, input)
	status, message := "completed", ""
	if err != nil {
		status = "failed"
		message = err.Error()
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
	}
	if err := s.m.pg.FinishFlowRun(id, status, result, message); err != nil {
		log.Printf("[flow] finish %s: %v", id, err)
	}
}
func (s *Server) consumeFlow(ctx context.Context, id string, compiled *flowproto.Compiled, input []byte) (string, error) {
	reader, err := compiled.Tool.StreamableCall(ctx, input)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	result := ""
	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if ev, ok := chunk.Content.(*event.Event); ok {
			if ev.Error != nil {
				_ = s.m.pg.AppendFlowEvent(id, ev.Object, ev.Author, ev.Error.Error())
				return result, ev.Error
			}
			switch ev.Object {
			case graph.ObjectTypeGraphNodeStart, graph.ObjectTypeGraphNodeComplete, graph.ObjectTypeGraphNodeError:
				if err := s.m.pg.AppendFlowEvent(id, ev.Object, ev.Author, ""); err != nil {
					return result, err
				}
			}
			for _, choice := range ev.Choices {
				if choice.Message.Role == "assistant" && choice.Message.Content != "" {
					result = choice.Message.Content
				}
			}
		} else {
			result = toolkit.TextOf(chunk.Content)
		}
	}
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
		events, err := s.m.pg.FlowEvents(id, since)
		if err != nil {
			return
		}
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, data)
			since = ev.ID
		}
		if run.Status != "running" && len(events) < 500 {
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
	rows, err := s.m.pg.ListFlows()
	if err != nil {
		return nil
	}
	allowed := pool
	if agent.ToolResolve != nil {
		allowed = agent.ToolResolve(ctx, key, pool)
	}
	deps := map[string]actool.CallableTool{"inspect": flowInspect()}
	palette := s.flowPool()
	deps["merge_subdomains"] = palette["merge_subdomains"]
	// These fixed CLI adapters share the native host executor with Bash.
	// An agent without the Bash capability must not gain it via a Flow.
	if bash, err := s.m.pg.GetTool("Bash"); err == nil && (bash == nil || (bash.Enabled && contains(bash.Agents, key))) {
		for _, name := range []string{"subfinder", "amass", "httpx", "ehole"} {
			deps[name] = palette[name]
		}
	}
	for _, item := range allowed {
		if palette[item.Declaration().Name] != nil {
			deps[item.Declaration().Name] = item
		}
	}
	out := []actool.CallableTool{}
	for _, row := range rows {
		if !contains(row.Agents, key) || len(row.Published) == 0 || string(row.Published) == "null" {
			continue
		}
		var def flowproto.Definition
		if json.Unmarshal(row.Published, &def) != nil {
			continue
		}
		compiled, err := flowproto.Compile(def, deps)
		if err != nil {
			continue
		}
		out = append(out, compiled.Tool)
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
