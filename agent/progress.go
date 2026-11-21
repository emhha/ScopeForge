package agent

import (
	"encoding/json"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/google/uuid"
)

// A message keeps its identity through generation and completion. Each emitted
// revision is a full snapshot, so live delivery and replay use the same contract.
type messageProgress struct {
	id       string
	revision int
	size     int
	emitted  time.Time
	complete bool
}

func (p *messageProgress) metadata(complete bool) json.RawMessage {
	if p.id == "" {
		p.id = uuid.NewString()
	}
	p.revision++
	data, _ := json.Marshal(map[string]any{"message": map[string]any{"id": p.id, "revision": p.revision, "complete": complete}})
	return data
}

func (p *messageProgress) emit(kind, body string, complete bool, record func(db.Activity)) {
	if body == "" || (p.complete && p.size == len(body)) {
		return
	}
	if !complete && p.id != "" && (p.size == len(body) || time.Since(p.emitted) < 250*time.Millisecond) {
		return
	}
	record(db.Activity{Kind: kind, Summary: firstLine(body, 200), Detail: body, Metadata: p.metadata(complete)})
	p.size, p.emitted, p.complete = len(body), time.Now(), complete
}
