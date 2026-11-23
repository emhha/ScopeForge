package agent

import (
	"context"
	"sync"

	"github.com/emhha/scopeforge/db"
)

type nodeSourcesKey struct{}
type nodeSourceBatchKey struct{}
type nodeSourceBatch struct{ refs []db.NodeSource }
type nodeSources struct {
	mu    sync.Mutex
	calls map[string][]*nodeSourceBatch
}

// Allocate when execution starts, not when its assistant/tool event is consumed.
// The runner can execute several turns before the activity consumer catches up,
// and providers may reuse a call ID in later turns. Keep an ordered batch per
// execution, including empty/denied calls, rather than accumulating by model ID.
func (s *nodeSources) begin(ctx context.Context, call string) context.Context {
	batch := &nodeSourceBatch{}
	s.mu.Lock()
	s.calls[call] = append(s.calls[call], batch)
	s.mu.Unlock()
	return context.WithValue(ctx, nodeSourceBatchKey{}, batch)
}

// Only actual successful domain writes author provenance; never parse model text.
func noteNodeSources(ctx context.Context, relation string, ids ...int64) {
	sources, _ := ctx.Value(nodeSourcesKey{}).(*nodeSources)
	batch, _ := ctx.Value(nodeSourceBatchKey{}).(*nodeSourceBatch)
	if sources == nil || batch == nil {
		return
	}
	sources.mu.Lock()
	defer sources.mu.Unlock()
	for _, id := range ids {
		if id > 0 {
			batch.refs = append(batch.refs, db.NodeSource{NodeID: id, Relation: relation})
		}
	}
}
func (s *nodeSources) take(call string) []db.NodeSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.calls[call]
	if len(queue) == 0 {
		return nil
	}
	out := append([]db.NodeSource(nil), queue[0].refs...)
	if len(queue) == 1 {
		delete(s.calls, call)
	} else {
		s.calls[call] = queue[1:]
	}
	return out
}
