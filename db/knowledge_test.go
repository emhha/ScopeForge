package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLearningUpgradePreservesExistingV3Data(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetSetting("learning-migration-fixture", "preserved"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`DROP TABLE knowledge_documents; DROP TABLE memory_promotions; PRAGMA user_version=3`)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if value, _, err := d.GetSetting("learning-migration-fixture"); err != nil || value != "preserved" {
		t.Fatalf("existing settings: %q %v", value, err)
	}
	if err := d.AddKnowledgeDocument(KnowledgeDocument{ID: "test", Name: "guide.md", SHA256: "fixture", Size: 10}); err != nil {
		t.Fatal(err)
	}
	docs, err := d.KnowledgeDocuments()
	if err != nil || len(docs) != 1 || docs[0].Status != "queued" {
		t.Fatalf("documents: %+v %v", docs, err)
	}
	if _, err := d.Exec(`INSERT INTO memory_promotions(task_id,content) VALUES(1,'abstract experience')`); err != nil {
		t.Fatal(err)
	}
}
