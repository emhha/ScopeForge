package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestNodeSourcesUpgradePreservesSQLiteV2Data(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetSetting("migration-fixture", "preserved"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`DROP TABLE node_sources; PRAGMA user_version=2`)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	value, _, err := d.GetSetting("migration-fixture")
	if err != nil || value != "preserved" {
		t.Fatal(value, err)
	}
	var version, count int
	if err = d.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != sqliteSchemaVersion {
		t.Fatal(version, err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM node_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestNodeSourceProvenanceSurvivesTaskArchiveRestore(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("archive lineage", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(task.ExplorationID)
	node, err := store.AddNode(KindFinding, map[string]any{"summary": "fixture finding"}, 5, "confirmed", "mainagent", nil)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := store.AppendActivity(Activity{Worker: "mainagent", Kind: "tool_result", ToolUseID: "archive-call", NodeSources: []NodeSource{{NodeID: node, Relation: "produced"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	job, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["node_sources"] != 1 {
		t.Fatalf("missing archive provenance: %+v", snapshot.DataCounts)
	}
	if err = d.CompleteTaskArchive(job.ID, snapshot, "/tmp/fixture.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RestoreTaskArchive(job.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.FindingLineageSessions(node)
	if err != nil || len(sessions) != 1 || len(sessions[0].Contributions) != 1 || sessions[0].Contributions[0].Seq != seq {
		t.Fatalf("lost restored provenance: %+v %v", sessions, err)
	}
}
