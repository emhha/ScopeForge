// Package sidequestion captures immutable main-agent checkpoints. It never owns
// an agent session or a tool executor.
package sidequestion

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

type Parent struct {
	ConversationID int64 `json:"conversation_id,omitempty"`
	TaskID         int64 `json:"task_id,omitempty"`
	ExplorationID  int64 `json:"exploration_id,omitempty"`
	IntentID       int64 `json:"intent_id,omitempty"`
}

func (p Parent) Key() string {
	if p.ConversationID > 0 {
		return fmt.Sprintf("conv-%d", p.ConversationID)
	}
	if p.IntentID > 0 {
		return fmt.Sprintf("task-%d-exp-%d-worker-i%d", p.TaskID, p.ExplorationID, p.IntentID)
	}
	return fmt.Sprintf("task-%d-exp-%d-main", p.TaskID, p.ExplorationID)
}

// Model contains only configuration identity, never credentials or proxy URLs.
type Model struct {
	ProfileID    int64  `json:"profile_id"`
	Name         string `json:"name"`
	Format       string `json:"format"`
	Model        string `json:"model"`
	Identity     string `json:"identity"`
	Streaming    bool   `json:"streaming"`
	WindowTokens int    `json:"window_tokens"`
}

type Snapshot struct {
	Parent     Parent        `json:"parent"`
	RunID      int64         `json:"run_id"`
	Version    int64         `json:"version"`
	CapturedAt time.Time     `json:"captured_at"`
	Model      Model         `json:"model"`
	Request    model.Request `json:"request"`
}

// Native request JSON preserves message/provider data and generation settings.
// Tools and HTTP headers are deliberately absent: a checkpoint is evidence,
// never a retained capability or transport credential. Answer always has no tools.
func CloneRequest(req model.Request) (model.Request, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return model.Request{}, err
	}
	var out model.Request
	err = json.Unmarshal(data, &out)
	return out, err
}

type Publisher func(Snapshot)
type publisherKey struct{}
type captureKey struct{}

func WithPublisher(ctx context.Context, publish Publisher) context.Context {
	return context.WithValue(ctx, publisherKey{}, publish)
}

type Capture struct {
	mu      sync.Mutex
	parent  Parent
	runID   int64
	version int64
	publish Publisher
}

func Attach(ctx context.Context, parent Parent) context.Context {
	publish, _ := ctx.Value(publisherKey{}).(Publisher)
	if publish == nil {
		return ctx
	}
	return context.WithValue(ctx, captureKey{}, &Capture{parent: parent, runID: time.Now().UnixNano(), publish: publish})
}
func (c *Capture) save(req model.Request, identity Model) {
	copy, err := CloneRequest(req)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.version++
	s := Snapshot{Parent: c.parent, RunID: c.runID, Version: c.version, CapturedAt: time.Now().UTC(), Model: identity, Request: copy}
	c.mu.Unlock()
	c.publish(s)
}

type boundModel struct {
	inner    model.Model
	identity Model
}

func Bind(inner model.Model, identity Model) model.Model {
	bound := &boundModel{inner: inner, identity: identity}
	if projector, ok := inner.(model.ProviderDataProjector); ok {
		return &projectingModel{boundModel: bound, ProviderDataProjector: projector}
	}
	return bound
}

type projectingModel struct {
	*boundModel
	model.ProviderDataProjector
}

func (m *boundModel) Info() model.Info { return m.inner.Info() }
func (m *boundModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	c, _ := ctx.Value(captureKey{}).(*Capture)
	if c == nil || req == nil || isSummary(*req) {
		return m.inner.GenerateContent(ctx, req)
	}
	c.save(*req, m.identity)
	checkpoint, err := CloneRequest(*req)
	if err != nil {
		return m.inner.GenerateContent(ctx, req)
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	source, err := m.inner.GenerateContent(attemptCtx, req)
	if source == nil {
		cancel()
		return nil, err
	}
	out := make(chan *model.Response)
	go func(startupErr error) {
		defer close(out)
		defer func() {
			cancel()
			for range source {
			}
		}()
		complete, failed := false, startupErr != nil
		var assistant model.Message
		for ev := range source {
			if ev != nil {
				failed = failed || ev.Error != nil
				complete = complete || ev.Done
				if !ev.IsPartial {
					for _, choice := range ev.Choices {
						if choice.Message.Role == model.RoleAssistant {
							assistant = choice.Message
						}
					}
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		if complete && !failed && ctx.Err() == nil && assistant.Role == model.RoleAssistant {
			checkpoint.Messages = append(checkpoint.Messages, assistant)
			c.save(checkpoint, m.identity)
		}
	}(err)
	return out, err
}
