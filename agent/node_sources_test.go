package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNativeToolWritesRecordExactProvenanceWithReusedCallIDs(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("native provenance", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(task.ExplorationID)
	intent, err := store.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(store, "work#1")
	turns := 0
	provider := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		turns++
		if turns <= 2 {
			args, _ := json.Marshal(map[string]any{"summary": fmt.Sprintf("actual fact %d", turns), "intent_id": intent, "confidence": "observed", "evidence": "fixture"})
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "reused-call", Type: "function", Function: model.FunctionDefinitionParam{Name: "record_fact", Arguments: args}}}}}}}), nil
		}
		return modeltest.Responses(modeltest.Final("done")), nil
	})
	var records []db.Activity
	_, _, err = captureRun(t.Context(), RunConfig{Model: provider, Tools: []tool.CallableTool{ts.recordFact()}, DisableHostTools: true, MaxTurns: 4}, "record twice", func(a db.Activity) {
		if a.Kind != "tool_result" {
			return
		}
		a.NodeID = &intent
		a.Worker = "work#1"
		seq, e := store.AppendActivity(a)
		if e != nil {
			t.Error(e)
		}
		a.ID = seq
		records = append(records, a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("results=%+v", records)
	}
	for i, a := range records {
		if len(a.NodeSources) != 1 {
			t.Fatalf("result %d missing exact host provenance: %+v", i, a)
		}
		source := a.NodeSources[0]
		if source.Relation != "produced" {
			t.Fatalf("source=%+v", source)
		}
		sessions, e := store.FindingLineageSessions(source.NodeID)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, session := range sessions {
			for _, c := range session.Contributions {
				if c.Relation == "produced" && c.Precision == "event" && c.Seq == a.ID {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("source lost: %+v", sessions)
		}
	}
	if records[0].NodeSources[0].NodeID == records[1].NodeSources[0].NodeID {
		t.Fatal("reused call ID retained the prior write")
	}
}
