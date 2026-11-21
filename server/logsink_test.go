package server

import (
	"log"
	"testing"

	agentlog "trpc.group/trpc-go/trpc-agent-go/log"
)

func TestLogCaptureIncludesAgentSDK(t *testing.T) {
	previous, previousContext, output := agentlog.Default, agentlog.ContextDefault, log.Writer()
	t.Cleanup(func() { agentlog.Default = previous; agentlog.ContextDefault = previousContext; log.SetOutput(output) })
	_, cursor := logSink.recent(0, 1)
	StartLogCapture()
	agentlog.Errorf("[agent-test] retrieval failed")
	agentlog.WarnContext(t.Context(), "[agent-context-test] retry")
	lines, _ := logSink.recent(cursor, 10)
	if len(lines) != 2 || lines[0].Level != "error" || lines[1].Level != "warn" {
		t.Fatalf("SDK logs missing: %+v", lines)
	}
}
