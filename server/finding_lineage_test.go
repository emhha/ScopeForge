package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/emhha/scopeforge/db"
)

func TestFindingLineageAPIIncludesSourceSessions(t *testing.T) {
	s, f, request := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal("missing finding task", err)
	}
	taskID := fmt.Sprint(*finding.TaskID)
	task := s.m.ResolveTask(taskID)
	if task == nil {
		t.Fatal("missing task")
	}
	seg := 2
	seq, err := task.Store.AppendActivity(db.Activity{Kind: "tool_result", Worker: "mainagent", MainSeg: &seg, ToolUseID: "source-report", NodeSources: []db.NodeSource{{NodeID: f.NodeID, Relation: "produced"}}})
	if err != nil {
		t.Fatal(err)
	}
	response := request("GET", fmt.Sprintf("/api/exploration/findings/%d/lineage", f.FindingID), "")
	if response.Code != 200 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var result struct {
		Nodes    []json.RawMessage   `json:"nodes"`
		Edges    []json.RawMessage   `json:"edges"`
		Sessions []db.LineageSession `json:"sessions"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) == 0 || len(result.Sessions) != 1 {
		t.Fatalf("response %s", response.Body.String())
	}
	session := result.Sessions[0]
	if session.TaskID != taskID || session.Session != "main:2" || session.Role != "mainagent" || len(session.Contributions) != 1 {
		t.Fatalf("session %+v", session)
	}
	contribution := session.Contributions[0]
	if contribution.NodeID != fmt.Sprint(f.NodeID) || contribution.Seq != seq || contribution.ToolUseID != "source-report" || contribution.Precision != "event" || contribution.Relation != "produced" {
		t.Fatalf("contribution %+v", contribution)
	}
}

func TestFindingLineageAPIEmptySessionsIsArray(t *testing.T) {
	s, f, request := trafficEvidenceServer(t)
	for _, detached := range []bool{false, true} {
		if detached {
			if _, err := s.m.pg.Exec(`UPDATE findings SET node_id=NULL WHERE id=$1`, f.FindingID); err != nil {
				t.Fatal(err)
			}
		}
		response := request("GET", fmt.Sprintf("/api/exploration/findings/%d/lineage", f.FindingID), "")
		if response.Code != 200 {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if string(result["sessions"]) != "[]" {
			t.Fatalf("sessions must be []: %s", response.Body.String())
		}
	}
}
