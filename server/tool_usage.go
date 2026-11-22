package server

import (
	"context"
	"log"

	actool "trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
)

type toolUsageRecorder interface {
	InsertToolUsage(*db.ToolUsage) error
}

// meteredTool records one row immediately before a catalog tool is invoked. It
// embeds the resolved tool so schema overrides, defaults and permission behavior
// remain unchanged.
type meteredTool struct {
	actool.CallableTool
	recorder toolUsageRecorder
	toolKey  string
	agentKey string
	ri       agent.RunInfo
}

func meterTool(t actool.CallableTool, recorder toolUsageRecorder, toolKey, agentKey string, ri agent.RunInfo) actool.CallableTool {
	if recorder == nil {
		return t
	}
	wrapped := &meteredTool{CallableTool: t, recorder: recorder, toolKey: toolKey, agentKey: agentKey, ri: ri}
	if stream, ok := t.(actool.StreamableTool); ok {
		return &meteredStreamTool{meteredTool: wrapped, stream: stream}
	}
	return wrapped
}

func (m *meteredTool) Call(ctx context.Context, input []byte) (any, error) {
	m.record()
	return m.CallableTool.Call(ctx, input)
}
func (m *meteredTool) record() {
	if err := m.recorder.InsertToolUsage(&db.ToolUsage{
		ToolKey:       m.toolKey,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
	}); err != nil {
		log.Printf("[toolusage] insert %s: %v", m.toolKey, err)
	}
}

type meteredStreamTool struct {
	*meteredTool
	stream actool.StreamableTool
}

func (m *meteredStreamTool) StreamableCall(ctx context.Context, input []byte) (*actool.StreamReader, error) {
	m.record()
	return m.stream.StreamableCall(ctx, input)
}
func (m *meteredStreamTool) StructuredStreamErrors() bool {
	if v, ok := m.stream.(interface{ StructuredStreamErrors() bool }); ok {
		return v.StructuredStreamErrors()
	}
	return false
}
