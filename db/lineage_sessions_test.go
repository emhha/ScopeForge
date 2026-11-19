package db

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func TestFindingLineageSessionProvenance(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "lineage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("lineage", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(task.ExplorationID)
	node := func(kind, summary string) int64 {
		t.Helper()
		state := "confirmed"
		if kind == KindIntent {
			state = "done"
		}
		id, err := s.AddNode(kind, map[string]any{"summary": summary}, 5, state, "work#1", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := func(a int64, rel string, b int64) {
		t.Helper()
		if err := s.Link(a, rel, b); err != nil {
			t.Fatal(err)
		}
	}
	appendA := func(a Activity) int64 {
		t.Helper()
		id, err := s.AppendActivity(a)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := node(KindIntent, "first intent")
	fact := node(KindFact, "first fact")
	second := node(KindIntent, "second intent")
	finding := node(KindFinding, "final finding")
	unrelated := node(KindIntent, "unrelated same worker")
	link(first, RelYields, fact)
	link(fact, RelDerivedFrom, second)
	link(second, RelYields, finding)
	seg := 3
	created := appendA(Activity{Worker: "mainagent", MainSeg: &seg, Kind: "tool_result", ToolUseID: "main-create", NodeSources: []NodeSource{{NodeID: first, Relation: "created"}}})
	appendA(Activity{Worker: "work#1", NodeID: &first, Kind: "result"})
	appendA(Activity{Worker: "work#1", NodeID: &unrelated, Kind: "result"})
	planned := appendA(Activity{Worker: "planner", Kind: "tool_result", ToolUseID: "plan-create", NodeSources: []NodeSource{{NodeID: second, Relation: "created"}}})
	produced := appendA(Activity{Worker: "work#1", NodeID: &second, Kind: "tool_result", ToolUseID: "actual-report", NodeSources: []NodeSource{{NodeID: finding, Relation: "produced"}}})
	got, err := s.FindingLineageSessions(finding)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("sessions: %+v", got)
	}
	byKey := map[string]LineageSession{}
	for _, session := range got {
		byKey[session.Session] = session
		if session.TaskID != fmt.Sprint(task.ID) {
			t.Fatalf("task id %+v", session)
		}
	}
	check := func(key, node, relation, precision string, seq int64) {
		t.Helper()
		for _, c := range byKey[key].Contributions {
			if c.NodeID == node && c.Relation == relation && c.Precision == precision && c.Seq == seq {
				return
			}
		}
		t.Fatalf("missing contribution %s %s %s: %+v", key, node, relation, got)
	}
	check("main:3", fmt.Sprint(first), "created", "event", created)
	check("plan", fmt.Sprint(second), "created", "event", planned)
	check(fmt.Sprint("intent:", first), fmt.Sprint(first), "executed", "session", 0)
	check(fmt.Sprint("intent:", first), fmt.Sprint(fact), "produced", "legacy", 0)
	check(fmt.Sprint("intent:", second), fmt.Sprint(finding), "produced", "event", produced)
	if _, ok := byKey[fmt.Sprint("intent:", unrelated)]; ok {
		t.Fatal("reused worker leaked unrelated intent")
	}
	for _, session := range got {
		for _, c := range session.Contributions {
			if c.Precision != "event" && (c.Seq != 0 || c.ToolUseID != "") {
				t.Fatal("false exact deep link")
			}
		}
	}
	empty, err := s.FindingLineageSessions(9999999)
	raw, _ := json.Marshal(empty)
	if err != nil || string(raw) != "[]" {
		t.Fatalf("empty=%s err=%v", raw, err)
	}
}

func TestAppendActivitySourcesAtomicAndExplorationScoped(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "sources.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	one, _ := d.CreateExploration("one", "one")
	two, _ := d.CreateExploration("two", "two")
	s := d.Exploration(one)
	good, _ := s.AddNode(KindFact, map[string]any{}, 1, "confirmed", "", nil)
	foreign, _ := d.Exploration(two).AddNode(KindFact, map[string]any{}, 1, "confirmed", "", nil)
	_, err = s.AppendActivity(Activity{Kind: "tool_result", NodeSources: []NodeSource{{NodeID: good, Relation: "produced"}, {NodeID: foreign, Relation: "produced"}}})
	if err == nil {
		t.Fatal("foreign source accepted")
	}
	for _, table := range []string{"activity", "node_sources"} {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rollback count %d err %v", table, n, err)
		}
	}
	id, err := s.AppendActivity(Activity{Kind: "tool_result", NodeSources: []NodeSource{{NodeID: good, Relation: "produced"}, {NodeID: good, Relation: "produced"}}})
	if err != nil {
		t.Fatal(err)
	}
	var persisted int64
	if err = d.QueryRow(`SELECT activity_id FROM node_sources WHERE node_id=$1`, good).Scan(&persisted); err != nil || persisted != id {
		t.Fatalf("source activity %d want %d err %v", persisted, id, err)
	}
	if _, err = d.Exec(`DELETE FROM activity WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = d.QueryRow(`SELECT count(*) FROM node_sources`).Scan(&count)
	if count != 0 {
		t.Fatal("activity deletion left source links")
	}
}

func TestNodeSourcesMigrationFromVersionTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-feature schema; reopen must create the table transactionally.
	if _, err = d.Exec(`DROP TABLE node_sources; PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM node_sources`).Scan(&count); err != nil {
		t.Fatal(err)
	}
}
