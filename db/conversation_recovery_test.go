package db

import (
	"path/filepath"
	"testing"
)

func TestConversationMetadataAndProfileThinkingSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "business.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := &LLMProfile{Name: "fixture", Format: "anthropic", Model: "claude-test", APIKey: "fixture", ThinkingType: "enabled", ThinkingBudget: 4096}
	id, err := d.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = id
	profile.APIKey = ""
	profile.ThinkingBudget = 8192
	if _, err = d.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	profile.APIKey = "replacement"
	profile.ThinkingBudget = 6144
	if _, err = d.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	c, err := d.CreateConversation("auto", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	const metadata = `{"type":"model_call","system_prompt":"original captured prompt"}`
	seq, err := d.AppendConvActivity(c.ID, Activity{Kind: "model_call", Summary: "captured call", Detail: "full wire detail", Metadata: []byte(metadata)})
	if err != nil {
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
	p, err := d.ProfileByID(id)
	if err != nil || p.ThinkingBudget != 6144 || p.APIKey != "replacement" {
		t.Fatalf("profile=%+v err=%v", p, err)
	}
	listed, err := d.ListProfiles()
	if err != nil || len(listed) != 1 || listed[0].ThinkingBudget != 6144 {
		t.Fatalf("profiles=%+v err=%v", listed, err)
	}
	tail, _, err := d.ConvActivityList(c.ID, 0, 10)
	if err != nil || len(tail) != 1 || string(tail[0].Metadata) != metadata {
		t.Fatalf("tail=%+v err=%v", tail, err)
	}
	page, _, err := d.ConvActivityPage(c.ID, 0, 10)
	if err != nil || len(page) != 1 || string(page[0].Metadata) != metadata {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	detail, err := d.ConvActivityDetail(c.ID, seq)
	if err != nil || detail != "full wire detail" {
		t.Fatalf("detail=%s err=%v", detail, err)
	}
}

func TestVersionFiveUpgradeRetainsBusinessData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-business.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.CreateConversation("auto", "before upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.AppendConvActivity(c.ID, Activity{Kind: "text", Summary: "existing history"}); err != nil {
		t.Fatal(err)
	}
	// Recreate the two missing columns of a v5 installation without importing any
	// user files or retaining a second SQLite driver solely for a test fixture.
	if _, err = d.Exec(`DROP INDEX idx_conv_message_revision; DROP INDEX idx_activity_message_revision; ALTER TABLE conversation_activities DROP COLUMN metadata; ALTER TABLE llm_profiles DROP COLUMN thinking_budget; PRAGMA user_version=5`); err != nil {
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
	page, _, err := d.ConvActivityPage(c.ID, 0, 10)
	if err != nil || len(page) != 1 || page[0].Summary != "existing history" || string(page[0].Metadata) != "{}" {
		t.Fatalf("migration damaged history: %+v err=%v", page, err)
	}
	if _, err = d.SaveProfile(&LLMProfile{Name: "upgraded", Format: "anthropic", Model: "fixture", ThinkingBudget: 4096}); err != nil {
		t.Fatal(err)
	}
}
