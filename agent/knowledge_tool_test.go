package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/emhha/scopeforge/toolkit"
	knowledgetool "trpc.group/trpc-go/trpc-agent-go/knowledge/tool"
)

func TestKnowledgeMissIsNormalAndFailuresRemainErrors(t *testing.T) {
	for _, message := range []string{"no relevant information found", "knowledge storage unavailable"} {
		cause := errors.New(message)
		native := toolkit.New("knowledge_search", "test", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) { return toolkit.Output{}, cause })
		result, err := (knowledgeSearchTool{native}).Call(t.Context(), []byte(`{"query":"missing"}`))
		if message == "no relevant information found" {
			response, ok := result.(*knowledgetool.KnowledgeSearchResponse)
			if err != nil || !ok || response.Documents == nil || len(response.Documents) != 0 {
				t.Fatalf("miss: %#v %v", result, err)
			}
		} else if !errors.Is(err, cause) {
			t.Fatalf("lost retrieval error: %v", err)
		}
	}
}
