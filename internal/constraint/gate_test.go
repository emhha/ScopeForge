package constraint

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scopeforge/internal/reasonix/provider"
)

// fakeStreamProvider 计数到达的 Stream 调用(并发闸门测试)。
type fakeStreamProvider struct {
	name     string
	arrivals atomic.Int64 // 累计到达请求数
	delay    time.Duration
	inflight chan struct{} // 非 nil 时 Stream 阻塞等待释放(精确并发控制)
}

func (f *fakeStreamProvider) Name() string { return f.name }

func (f *fakeStreamProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	f.arrivals.Add(1)
	if f.inflight != nil {
		select {
		case <-f.inflight:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	out := make(chan provider.Chunk)
	close(out)
	return out, nil
}

// TestEndpointGateMaxConcurrent 并发闸门:同时最多 maxConcurrent 个请求。
func TestEndpointGateMaxConcurrent(t *testing.T) {
	inner := &fakeStreamProvider{name: "inner", inflight: make(chan struct{})}
	gate := NewEndpointGate(inner, 2, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 并发发 5 个请求(全部阻塞在并发槽或 inflight)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := gate.Stream(ctx, provider.Request{})
			if err != nil {
				t.Errorf("stream: %v", err)
			}
			for range ch {
			}
		}()
	}
	// 等前 2 个进入内层(并发槽 2)
	deadline := time.Now().Add(3 * time.Second)
	for inner.arrivals.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("maxSeen=%d, want >= 2 (concurrency limit)", inner.arrivals.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if inner.arrivals.Load() >= 3 {
		t.Fatalf("maxSeen=%d, want 2 (semaphore not limiting)", inner.arrivals.Load())
	}
	// 释放一个槽 → 第三个进入
	inner.inflight <- struct{}{}
	deadline = time.Now().Add(3 * time.Second)
	for inner.arrivals.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("3rd stream not started after slot release")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 清理:释放其余槽
	close(inner.inflight)
	wg.Wait()
}

// TestEndpointGateMinInterval 最小间隔:两个请求间隔 ≥ min_interval。
func TestEndpointGateMinInterval(t *testing.T) {
	inner := &fakeStreamProvider{name: "inner"}
	gate := NewEndpointGate(inner, 0, 200*time.Millisecond)

	start := time.Now()
	ch1, err := gate.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch1 {
	}
	ch2, _ := gate.Stream(context.Background(), provider.Request{})
	for range ch2 {
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 200ms (min_interval)", elapsed)
	}
}
