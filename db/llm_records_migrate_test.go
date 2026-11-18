package db

import (
	"path/filepath"
	"testing"
)

func TestLLMRecordsRawBodiesSurviveSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := &LLMRecord{Model: "fixture", RawRequest: `{"a":1}`, RawResponse: "data: x\n\n"}
	if err := d.InsertLLMRecord(original); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	records, total, err := d.ListLLMRecords("fixture", "", "", 0, 10)
	if err != nil || total != 1 || len(records) != 1 {
		t.Fatalf("records=%v total=%d error=%v", records, total, err)
	}
	got, err := d.GetLLMRecord(records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RawRequest != original.RawRequest || got.RawResponse != original.RawResponse {
		t.Fatal("raw transport bodies changed across reopen")
	}
}
