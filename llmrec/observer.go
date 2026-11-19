package llmrec

import (
	"context"
	"time"
)

// CallRecord snapshots one actual provider attempt independently of optional
// request/response body recording. SystemPrompt is detail-only and must never
// be serialized into the lightweight activity metadata.
type CallRecord struct {
	SystemPrompt string    `json:"-"`
	RequestID    string    `json:"request_id"`
	ProfileID    int64     `json:"profile_id,omitempty"`
	ProfileName  string    `json:"profile_name"`
	Model        string    `json:"model"`
	Role         string    `json:"role,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`
	Status       string    `json:"status"`
	Input        *int      `json:"input_tokens,omitempty"`
	Output       *int      `json:"output_tokens,omitempty"`
}

type callObserverKey struct{}

func WithCallObserver(ctx context.Context, observe func(CallRecord)) context.Context {
	return context.WithValue(ctx, callObserverKey{}, observe)
}

// WithProfileID is applied during construction, before publishing the recorder.
func (r *Recorder) WithProfileID(id int64) *Recorder {
	r.profileID = id
	return r
}
