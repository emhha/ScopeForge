package db

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestLegacyLineageDoesNotInventExecutionOrMainPlannerSources(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("legacy", "legacy", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(task.ExplorationID)
	intent, err := s.AddIntent(map[string]any{"summary": "pending intent"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	finding, err := s.AddNode(KindFinding, map[string]any{"summary": "legacy finding"}, 5, "confirmed", "mainagent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Link(intent, RelYields, finding); err != nil {
		t.Fatal(err)
	}
	// Neither general system state messages nor unrelated main/planner activity
	// proves who authored an intent/finding or that the pending intent ran.
	for _, a := range []Activity{{Worker: "work#1", NodeID: &intent, Kind: "system"}, {Worker: "planner", Kind: "result"}, {Worker: "mainagent", Kind: "result"}} {
		if _, err = s.AppendActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := s.FindingLineageSessions(finding)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Session != fmt.Sprint("intent:", intent) || len(sessions[0].Contributions) != 1 {
		t.Fatalf("invented sessions/execution: %+v", sessions)
	}
	c := sessions[0].Contributions[0]
	if c.Relation != "produced" || c.Precision != "legacy" || c.Seq != 0 || c.ToolUseID != "" {
		t.Fatalf("legacy must not claim exact actor %+v", c)
	}
}
