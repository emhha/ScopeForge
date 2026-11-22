package db

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestFlowsSurviveReopenAndUpgradeFromVersionEight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flows.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate a v8 database, then exercise the actual startup migration.
	if _, err = d.Exec(`DROP TABLE flow_run_events; DROP TABLE flow_runs; DROP TABLE flows; PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"name":"demo","version":1}`)
	id, err := d.SaveFlow(0, "demo", definition, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PublishFlow(id, 1, definition, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SaveFlow(id, "demo", definition, 0); !errors.Is(err, ErrFlowConflict) {
		t.Fatalf("stale save err=%v", err)
	}
	if err = d.CreateFlowRun("run", id, definition, json.RawMessage(`{"target":"fixture"}`)); err != nil {
		t.Fatal(err)
	}
	if err = d.AppendFlowEvent("run", "graph.node.start", "node_begin", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishFlowRun("run", "completed", `{"output":"ok"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	flow, err := d.GetFlow(id)
	if err != nil || flow.Agents[0] != "auto" || string(flow.Published) != string(definition) {
		t.Fatalf("flow=%+v err=%v", flow, err)
	}
	runs, err := d.ListFlowRuns(id)
	if err != nil || len(runs) != 1 || runs[0].Status != "completed" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	events, err := d.FlowEvents("run", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	tail, err := d.FlowEvents("run", events[0].ID)
	if err != nil || len(tail) != 0 {
		t.Fatalf("replay cursor duplicated events: %+v %v", tail, err)
	}
	run, err := d.GetFlowRun("run")
	if err != nil || run.Result != `{"output":"ok"}` {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}
