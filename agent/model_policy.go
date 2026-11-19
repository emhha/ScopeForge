package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emhha/scopeforge/llmpool"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func retryNative(err error) bool {
	if err == nil || IsQuotaExhaustedMessage(err.Error()) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if status := llmpool.HTTPStatus(err); status != 0 {
		return status == 408 || status == 429 || status >= 500
	}
	return true
}
func waitNative(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
func (m *configuredModel) throttle(ctx context.Context) error {
	interval := time.Duration(0)
	if m.config.RatePerSecond > 0 {
		interval = time.Duration(float64(time.Second) / m.config.RatePerSecond)
	}
	if m.config.RatePerMinute > 0 {
		interval = max(interval, time.Duration(float64(time.Minute)/m.config.RatePerMinute))
	}
	if interval <= 0 {
		return ctx.Err()
	}
	m.rateMu.Lock()
	slot := maxTime(time.Now(), m.next)
	m.next = slot.Add(interval)
	m.rateMu.Unlock()
	if !waitNative(ctx, time.Until(slot)) {
		return ctx.Err()
	}
	return nil
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func (m *configuredModel) generate(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(chan *model.Response)
	connections := m.config.Retry.ConnectAttempts
	if connections == 0 {
		connections = 3
	}
	connections = max(0, connections)
	empties := m.config.Retry.EmptyAttempts
	if empties == 0 {
		empties = 2
	}
	empties = max(0, empties)
	go func() {
		defer close(out)
		send := func(ev *model.Response) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		failure := func(err error) {
			send(&model.Response{Error: &model.ResponseError{Type: model.ErrorTypeAPIError, Message: err.Error()}, Done: true})
		}
		for connection, empty := 0, 0; ; {
			if err := m.throttle(ctx); err != nil {
				return
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			source, err := m.inner.GenerateContent(attemptCtx, req)
			committed := false
			var pending []*model.Response
			if source != nil {
				for ev := range source {
					if ev == nil {
						continue
					}
					if ev.Error != nil {
						err = ev.Error
						continue
					}
					if err != nil {
						continue
					}
					if !committed && !llmpool.ResponseCommitsOutput(ev) {
						pending = append(pending, ev)
						continue
					}
					if !committed {
						for _, buffered := range pending {
							if !send(buffered) {
								cancel()
							}
						}
						pending = nil
						committed = true
					}
					if !send(ev) {
						cancel()
					}
				}
			} else if err == nil {
				err = fmt.Errorf("model returned nil response channel")
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			if committed {
				if err != nil {
					failure(err)
				}
				return
			}
			delay := time.Duration(0)
			retry := false
			if err != nil && connection < connections && retryNative(err) {
				delay = m.config.Retry.ConnectInterval
				if delay <= 0 {
					delay = min(8*time.Second, 500*time.Millisecond*time.Duration(1<<min(connection, 4)))
				}
				connection++
				retry = true
			}
			if err == nil && empty < empties {
				delay = m.config.Retry.EmptyInterval
				if delay <= 0 {
					delay = min(8*time.Second, 500*time.Millisecond*time.Duration(1<<min(empty, 4)))
				}
				empty++
				retry = true
			}
			if retry {
				if !waitNative(ctx, delay) {
					return
				}
				continue
			}
			for _, buffered := range pending {
				if !send(buffered) {
					return
				}
			}
			if err != nil {
				failure(err)
			}
			return
		}
	}()
	return out, nil
}
