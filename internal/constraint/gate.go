package constraint

import (
	"context"
	"sync"
	"time"

	"scopeforge/internal/reasonix/provider"
)

// EndpointGate 是端点级并发闸门(docs/03 §7 成本护栏,⚑ LingXi `_EndpointGate`):
//
//	BoundedSemaphore(并发上限) + min_interval(请求最小间隔)。
//	包装 provider,在调用内层 Stream 前限流,慢端点场景防止并发风暴烧钱。
type EndpointGate struct {
	inner       provider.Provider
	sem         chan struct{}
	mu          sync.Mutex
	minInterval time.Duration
	last        time.Time
}

// NewEndpointGate 构建端点闸门。maxConcurrent<=0 时不限并发;minInterval<=0 时不限间隔。
func NewEndpointGate(inner provider.Provider, maxConcurrent int, minInterval time.Duration) *EndpointGate {
	g := &EndpointGate{inner: inner, minInterval: minInterval}
	if maxConcurrent > 0 {
		g.sem = make(chan struct{}, maxConcurrent)
	}
	return g
}

// Name 透传内层 provider 名。
func (g *EndpointGate) Name() string { return g.inner.Name() }

// Stream 限流后转发内层流。返回的 channel 在流结束时释放并发槽。
func (g *EndpointGate) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	if g.sem != nil {
		select {
		case g.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 最小间隔(串行化节流)
	if g.minInterval > 0 {
		g.mu.Lock()
		wait := g.minInterval - time.Since(g.last)
		if wait > 0 {
			g.mu.Unlock()
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				if g.sem != nil {
					<-g.sem
				}
				return nil, ctx.Err()
			}
			g.mu.Lock()
			g.last = time.Now()
			g.mu.Unlock()
		} else {
			g.last = time.Now()
			g.mu.Unlock()
		}
	}
	inner, err := g.inner.Stream(ctx, req)
	if err != nil {
		if g.sem != nil {
			<-g.sem
		}
		return nil, err
	}
	out := make(chan provider.Chunk)
	go func() {
		defer func() {
			if g.sem != nil {
				<-g.sem
			}
		}()
		for c := range inner {
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
		close(out)
	}()
	return out, nil
}
