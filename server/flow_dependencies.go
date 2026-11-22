package server

import (
	"encoding/json"
	"fmt"

	"github.com/emhha/scopeforge/internal/flowproto"
)

func (s *Server) checkWorkflowCycles(id int64, def flowproto.Definition, stack map[int64]bool) error {
	if len(stack) >= 8 {
		return fmt.Errorf("maximum workflow nesting depth reached")
	}
	if id != 0 {
		if stack[id] {
			return fmt.Errorf("recursive workflow dependency rejected")
		}
		stack[id] = true
		defer delete(stack, id)
	}
	for _, node := range def.Nodes {
		if node.Type != "tool" {
			continue
		}
		row, err := s.m.pg.GetTool(node.Tool)
		if err != nil {
			return err
		}
		if row == nil || row.Kind != "workflow" {
			continue
		}
		var ref struct {
			ID      int64 `json:"flow_id"`
			Version int64 `json:"version_id"`
		}
		if err := json.Unmarshal(row.Exec, &ref); err != nil {
			return err
		}
		v, err := s.m.pg.GetFlowVersion(ref.Version)
		if err != nil {
			return err
		}
		var child flowproto.Definition
		if err := json.Unmarshal(v.Definition, &child); err != nil {
			return err
		}
		if err := s.checkWorkflowCycles(ref.ID, child, stack); err != nil {
			return fmt.Errorf("%s: %w", node.ID, err)
		}
	}
	return nil
}
