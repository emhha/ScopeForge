package failover

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"scopeforge/internal/reasonix/provider"
)

// fakeProvider 可编程失败/成功。
type fakeProvider struct {
	name    string
	failErr error // Stream 连接级失败
	midErr  error // 流中失败
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	ch := make(chan provider.Chunk, 4)
	go func() {
		defer close(ch)
		ch <- provider.Chunk{Type: provider.ChunkText, Text: f.name + ":hello"}
		if f.midErr != nil {
			ch <- provider.Chunk{Type: provider.ChunkError, Err: f.midErr}
		} else {
			ch <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{PromptTokens: 1, CompletionTokens: 1}}
		}
	}()
	return ch, nil
}

func TestConnectFailover(t *testing.T) {
	primary := &fakeProvider{name: "p1", failErr: errors.New("gateway down")}
	backup := &fakeProvider{name: "p2"}
	p := New(primary, []provider.Provider{backup}, "")
	if p.Name() != "p1" {
		t.Fatalf("initial name=%s", p.Name())
	}
	ch, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for c := range ch {
		if c.Type == provider.ChunkText {
			text += c.Text
		}
		if c.Err != nil {
			t.Fatalf("unexpected mid error: %v", c.Err)
		}
	}
	if text != "p2:hello" {
		t.Errorf("got %q want p2:hello(failover)", text)
	}
	if p.CurrentName() != "p2" {
		t.Errorf("current=%s want p2", p.CurrentName())
	}
}

func TestMidStreamFailoverMarks(t *testing.T) {
	primary := &fakeProvider{name: "p1", midErr: errors.New("stream cut")}
	backup := &fakeProvider{name: "p2"}
	p := New(primary, []provider.Provider{backup}, "")
	ch, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	gotErr := false
	for c := range ch {
		if c.Err != nil {
			gotErr = true
		}
	}
	if !gotErr {
		t.Error("mid-stream error should surface to caller")
	}
	// 标记切换:下个请求用备份
	if p.CurrentName() != "p2" {
		t.Errorf("current=%s want p2", p.CurrentName())
	}
	ch2, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for c := range ch2 {
		if c.Type == provider.ChunkText {
			text += c.Text
		}
	}
	if text != "p2:hello" {
		t.Errorf("got %q", text)
	}
}

func TestAllFail(t *testing.T) {
	primary := &fakeProvider{name: "p1", failErr: errors.New("x")}
	backup := &fakeProvider{name: "p2", failErr: errors.New("y")}
	p := New(primary, []provider.Provider{backup}, "")
	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil {
		t.Fatal("all fail should error")
	}
}

func TestNoBackupPassthrough(t *testing.T) {
	primary := &fakeProvider{name: "p1"}
	p := New(primary, nil, "")
	ch, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for c := range ch {
		if c.Type == provider.ChunkText {
			text += c.Text
		}
	}
	if text != "p1:hello" {
		t.Errorf("got %q", text)
	}
}

func TestNilPrimary(t *testing.T) {
	if p := New(nil, nil, ""); p != nil {
		t.Error("nil primary should yield nil")
	}
}

// TestProbeFailoverFast 健康探测先行:主端点 500 → 立即切换(不等 provider 重试)。
func TestProbeFailoverFast(t *testing.T) {
	// 探测端点恒 500(网关故障)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway error", http.StatusInternalServerError)
	}))
	defer probe.Close()
	primary := &fakeProvider{name: "p1"}
	backup := &fakeProvider{name: "p2"}
	p := New(primary, []provider.Provider{backup}, probe.URL)
	start := time.Now()
	ch, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for c := range ch {
		if c.Type == provider.ChunkText {
			text += c.Text
		}
	}
	if text != "p2:hello" {
		t.Errorf("got %q want p2:hello", text)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("failover too slow: %v", time.Since(start))
	}
	if p.CurrentName() != "p2" {
		t.Errorf("current=%s", p.CurrentName())
	}
}
