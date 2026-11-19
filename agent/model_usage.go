package agent

import (
	"context"

	"github.com/Autumn-27/artex/modelutil"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// Meter before Runner event delivery: cancellation may legitimately drop SDK UI
// events, but tokens already reported by the provider still belong to this run.
type usageModel struct {
	inner model.Model
	turn  *turn
}

func (m *usageModel) Info() model.Info { return m.inner.Info() }
func (m *usageModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	m.turn.models.Add(1)
	source, err := m.inner.GenerateContent(ctx, req)
	if err != nil {
		m.turn.models.Done()
		return nil, err
	}
	if source == nil {
		m.turn.models.Done()
		return nil, nil
	}
	out := make(chan *model.Response)
	go func() {
		defer close(out)
		defer m.turn.models.Done()
		var usage model.Usage
		defer func() { m.turn.usageMu.Lock(); modelutil.AddUsage(&m.turn.usage, usage); m.turn.usageMu.Unlock() }()
		for ev := range source {
			if ev != nil && ev.Usage != nil {
				modelutil.MergeUsage(&usage, *ev.Usage)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}
