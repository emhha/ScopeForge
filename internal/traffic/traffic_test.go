package traffic

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"scopeforge/internal/event"
)

type recSink struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recSink) Emit(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func TestRedact(t *testing.T) {
	in := "Cookie: session=abc123secret; user=admin\nAuthorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def\nX-Data: hello"
	out := Redact(in)
	if strings.Contains(out, "abc123secret") {
		t.Errorf("cookie value leaked: %q", out)
	}
	if strings.Contains(out, "eyJhbGciOiJIUzI1NiJ9") {
		t.Errorf("bearer leaked: %q", out)
	}
	if !strings.Contains(out, "X-Data: hello") {
		t.Errorf("benign header lost: %q", out)
	}
}

func TestRecorderEvents(t *testing.T) {
	sink := &recSink{}
	r := NewRecorder(sink, "c1")
	r.Record(Flow{Method: "GET", Host: "target.example.com", Path: "/login", Status: 200, ReqHead: "Cookie: s=secret123"})
	if len(sink.events) != 1 {
		t.Fatalf("events=%d", len(sink.events))
	}
	e := sink.events[0]
	if e.Kind != event.KindTraffic || e.ChallengeID != "c1" {
		t.Errorf("event=%+v", e)
	}
	flows := r.Flows()
	if len(flows) != 1 || flows[0].FlowRef == "" {
		t.Fatalf("flows=%+v", flows)
	}
	if strings.Contains(flows[0].ReqHead, "secret123") {
		t.Error("stored flow not redacted")
	}
}

func TestProxyHTTPCapture(t *testing.T) {
	sink := &recSink{}
	rec := NewRecorder(sink, "c1")
	// 目标服务器
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "yes")
		fmt.Fprintf(w, "ok %s", r.URL.Path)
	}))
	defer backend.Close()

	proxy, err := StartProxy(rec, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(mustURL("http://" + proxy.Addr())),
	}}
	resp, err := client.Get(backend.URL + "/secret?q=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.Flows()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	flows := rec.Flows()
	if len(flows) != 1 {
		t.Fatalf("flows=%d want 1", len(flows))
	}
	f := flows[0]
	if f.Method != "GET" || f.Status != 200 || !strings.Contains(f.Path, "/secret") {
		t.Errorf("flow=%+v", f)
	}
	if f.Host == "" {
		t.Error("host missing")
	}
	if !strings.Contains(f.RespHead, "X-Backend: yes") {
		t.Errorf("resp head=%q", f.RespHead)
	}
	if len(sink.events) != 1 || sink.events[0].Kind != event.KindTraffic {
		t.Errorf("events=%+v", sink.events)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
