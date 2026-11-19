package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emhha/scopeforge/toolkit"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestHTTPGuidanceFollowsFinalBindings(t *testing.T) {
	oldResolve, oldAugment := ToolResolve, ToolAugment
	t.Cleanup(func() { ToolResolve, ToolAugment = oldResolve, oldAugment })
	ToolAugment = nil
	http := toolkit.New("http_request", "fixture", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) { return toolkit.Text("ok"), nil })
	readback := toolkit.Named(http, "traffic_get")
	ToolResolve = nil
	_, info, cleanup := AugmentTools(t.Context(), "worker", []actool.CallableTool{http})
	cleanup()
	if !strings.Contains(info.HTTPGuidance, "http_request") || strings.Contains(info.HTTPGuidance, "traffic_get") || strings.Contains(info.HTTPGuidance, "http_session_create") {
		t.Fatalf("guidance invents unavailable tools: %s", info.HTTPGuidance)
	}
	_, info, cleanup = AugmentTools(t.Context(), "mainagent", []actool.CallableTool{http, readback})
	cleanup()
	if !strings.Contains(info.HTTPGuidance, "traffic_get") {
		t.Fatal("missing permitted evidence readback")
	}
	ToolResolve = func(context.Context, string, []actool.CallableTool) []actool.CallableTool { return nil }
	tools, info, cleanup := AugmentTools(t.Context(), "worker", []actool.CallableTool{http, readback})
	defer cleanup()
	if len(tools) != 0 || info.HTTPGuidance != "" {
		t.Fatal("disabled HTTP tool was reintroduced or advertised")
	}
}
