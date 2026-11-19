package agent

import (
	"context"
	"sync"

	"github.com/emhha/scopeforge/db"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type nodeSourcesKey struct{}
type nodeSources struct {
	mu    sync.Mutex
	calls map[string][]db.NodeSource
}

// Only actual successful domain writes author provenance; never parse model text.
func noteNodeSources(ctx context.Context, relation string, ids ...int64) {
	sources, _ := ctx.Value(nodeSourcesKey{}).(*nodeSources)
	call, ok := tool.ToolCallIDFromContext(ctx)
	if sources == nil || !ok || call == "" {
		return
	}
	sources.mu.Lock()
	defer sources.mu.Unlock()
	for _, id := range ids {
		if id > 0 {
			sources.calls[call] = append(sources.calls[call], db.NodeSource{NodeID: id, Relation: relation})
		}
	}
}
func (s *nodeSources) take(call string) []db.NodeSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.calls[call]
	delete(s.calls, call)
	return out
}
