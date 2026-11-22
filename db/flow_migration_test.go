package db

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestWorkflowV9MigrationPreservesRunsAndAllowsInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"name":"preserved","version":1,"input_schema":{"type":"object"}}`)
	id, err := d.SaveFlow(0, "preserved", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateFlowRun("old-run", id, raw, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendFlowEvent("old-run", "graph.node.start", "begin", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFlowRun("old-run", "completed", `{"ok":true}`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DROP TABLE flow_artifacts; DROP TABLE flow_versions; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old, err := d.GetFlowRun("old-run")
	if err != nil || old.Result != `{"ok":true}` {
		t.Fatalf("lost old run: %+v %v", old, err)
	}
	events, err := d.FlowEvents("old-run", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("lost old events: %+v %v", events, err)
	}
	if err := d.CreateFlowRun("inline", 0, raw, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFlowRunContext("inline", "inline", "auto", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign keys broken by migration")
	}
}
