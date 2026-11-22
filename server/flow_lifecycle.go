package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
)

func (s *Server) initWorkflows() {
	agents := json.RawMessage(`["auto","mainagent","worker","pentest"]`)
	for _, item := range s.workflowControlTools("auto") {
		schema, _ := json.Marshal(item.Declaration().InputSchema)
		_ = s.m.pg.SeedTool(item.Declaration().Name, item.Declaration().Description, schema, agents)
	}
	// Upgrade the old dynamic published definitions exactly once, preserving any
	// already registered tool's on/off switch and binding on subsequent restarts.
	rows, _ := s.m.pg.ListFlows()
	for _, row := range rows {
		if len(row.Published) == 0 || string(row.Published) == "null" {
			continue
		}
		if existing, _ := s.m.pg.GetTool("flow_" + row.Name); existing != nil {
			continue
		}
		var def flowproto.Definition
		if json.Unmarshal(row.Published, &def) != nil {
			continue
		}
		// A published snapshot may precede the current draft revision.
		if err := s.m.pg.ImportFlowPublication(row.ID, row.Name, def.Version, row.Published, row.Agents); err != nil {
			log.Printf("[workflow] migrate publication %s: %v", row.Name, err)
		}
	}
}
func (s *Server) validateWorkflow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Definition flowproto.Definition `json:"definition"`
		Agent      string               `json:"agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Definition.Version == 0 {
		in.Definition.Version = 1
	}
	pool, close, err := s.flowToolPool(r.Context(), in.Agent)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer close()
	if _, err := flowproto.Compile(in.Definition, pool); err != nil {
		writeJSON(w, 200, map[string]any{"valid": false, "diagnostics": []string{err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true, "diagnostics": []string{}})
}
func (s *Server) workflowVersions(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.m.pg.FlowVersions(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) unpublishWorkflow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	flow, err := s.m.pg.GetFlow(id)
	if err != nil {
		writeErr(w, 404, "flow not found")
		return
	}
	if err := s.m.pg.UnpublishFlow(id, "flow_"+flow.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) rollbackWorkflow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Version int64 `json:"version_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	v, err := s.m.pg.GetFlowVersion(in.Version)
	if err != nil || v.FlowID != id {
		writeErr(w, 404, "version not found")
		return
	}
	if err := s.m.pg.RollbackFlowPublication(id, v); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	flow, _ := s.m.pg.GetFlow(id)
	writeJSON(w, 200, flow)
}
func (s *Server) inlineWorkflow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Definition flowproto.Definition `json:"definition"`
		Input      json.RawMessage      `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, reader, finish, err := s.beginWorkflow(s.ctx, 0, 0, in.Definition, in.Input, "inline", "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	go func() { result, err := s.pipeWorkflow(s.ctx, id, reader, nil); finish(result, err) }()
	writeJSON(w, 202, map[string]any{"id": id, "status": "running"})
}
func (s *Server) allWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.ListAllFlowRuns()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for i := range rows {
		if row, err := s.flowRun(rows[i].ID); err == nil {
			rows[i] = *row
		}
	}
	writeJSON(w, 200, rows)
}
func (s *Server) saveRunAsWorkflow(w http.ResponseWriter, r *http.Request) {
	run, err := s.flowRun(r.PathValue("run"))
	if err != nil {
		writeErr(w, 404, "run not found")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var def flowproto.Definition
	if json.Unmarshal(run.Definition, &def) != nil {
		writeErr(w, 500, "invalid definition")
		return
	}
	def.Name = in.Name
	def.Version = 1
	pool, close, err := s.flowToolPool(r.Context(), "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer close()
	if _, err := flowproto.Compile(def, pool); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	raw, _ := json.Marshal(def)
	id, err := s.m.pg.SaveFlow(0, def.Name, raw, 0)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	flow, _ := s.m.pg.GetFlow(id)
	writeJSON(w, 200, flow)
}

// Workflow tools can only change their enabled flag, binding, description and
// deferred visibility here. Their schema and executable version stay immutable.
func (s *Server) updateWorkflowTool(existing *db.Tool, req customToolReq) error {
	if req.Kind != "workflow" {
		return fmt.Errorf("workflow kind cannot be changed; edit it in the workflow page")
	}
	if req.Schema != nil && string(req.Schema) != "null" {
		a, _ := json.Marshal(toolkit.SchemaMap(toolkit.Schema(ensureSchema(req.Schema))))
		b, _ := json.Marshal(toolkit.SchemaMap(toolkit.Schema(ensureSchema(existing.Schema))))
		if string(a) != string(b) {
			return fmt.Errorf("workflow schema belongs to its published version")
		}
	}
	existing.Description = req.Description
	existing.Agents = req.Agents
	existing.Enabled = req.Enabled
	existing.Deferred = req.Deferred
	return s.m.pg.UpdateCustomTool(existing)
}
