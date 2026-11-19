package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/toolkit"
)

// The server-level ToolSet behind buildDomainReg carries a nil ExplorationStore,
// and the tools table can bind any of its tools to any agent — including agents
// that never run inside a task. Every domain tool must therefore survive being
// called with nil stores: a nil deref here runs on the harness's own goroutine,
// out of reach of every recover() in the server, and kills the whole process.
func TestDomainToolsSurviveNilStores(t *testing.T) {
	inputs := []string{
		`{}`,
		`{"id":379,"asset_id":1,"goal_id":1,"evidence_id":1,"intent_id":1,"node_id":1,"work_id":1,` +
			`"summary":"x","reason":"x","name":"x","severity":"low","vulnclass":"x","text":"x","q":"x"}`,
	}
	for _, tool := range NewToolSet(nil, "").AllDomainTools() {
		for _, in := range inputs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s panicked with nil stores on %s: %v", tool.Declaration().Name, in, r)
					}
				}()
				if _, err := tool.Call(context.Background(), json.RawMessage(in)); err != nil {
					t.Fatalf("%s returned a transport error: %v", tool.Declaration().Name, err)
				}
			}()
		}
	}
}

// node_detail is the one that took the process down; assert it now answers with a
// usable refusal rather than dying.
func TestExplorationToolRefusesWithoutTask(t *testing.T) {
	ts := NewToolSet(nil, "")
	res, err := ts.NodeDetailTool().Call(context.Background(), json.RawMessage(`{"id":379}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !toolkit.Failed(res) || !strings.Contains(toolkit.TextOf(res), "任务上下文") {
		t.Fatalf("want an explanatory tool error, got IsError=%v %q", toolkit.Failed(res), toolkit.TextOf(res))
	}
}

// A panicking tool must degrade to a tool error: the run continues and the process
// survives, instead of the supervisor restarting into the same crash on replay.
func TestGuardPanicConvertsPanicToToolError(t *testing.T) {
	boom := toolkit.New("boom", "", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) { panic("nil map write") })
	_, err := guardPanic(boom).Call(t.Context(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "nil map write") {
		t.Fatalf("panic was not reported: %v", err)
	}
}
