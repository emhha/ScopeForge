package listener

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"scopeforge/internal/event"
)

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestTCPCallback(t *testing.T) {
	m := NewManager(event.Discard, "c1", 0)
	l, err := m.Open(context.Background(), ProtoTCP, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()

	conn, err := net.Dial("tcp", l.Addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n")
	conn.Close()

	waitFor(t, func() bool {
		conns, _ := m.Poll(l.ID)
		return len(conns) == 1
	}, 2*time.Second)
	conns, _ := m.Poll(l.ID)
	if !strings.Contains(conns[0].Data, "GET /") {
		t.Errorf("data=%q", conns[0].Data)
	}
	if conns[0].Proto != "tcp" || conns[0].Remote == "" {
		t.Errorf("conn=%+v", conns[0])
	}
}

func TestHTTPCallback(t *testing.T) {
	m := NewManager(event.Discard, "c1", 0)
	l, err := m.Open(context.Background(), ProtoHTTP, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()

	url := fmt.Sprintf("http://%s/callback?flag=test", l.Addr)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitFor(t, func() bool {
		conns, _ := m.Poll(l.ID)
		return len(conns) == 1
	}, 2*time.Second)
	conns, _ := m.Poll(l.ID)
	c := conns[0]
	if c.Method != "GET" || c.Host == "" || !strings.Contains(c.Path, "callback") {
		t.Errorf("conn=%+v", c)
	}
}

func TestDNSCallback(t *testing.T) {
	m := NewManager(event.Discard, "c1", 0)
	l, err := m.Open(context.Background(), ProtoDNS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()

	// 构造 DNS 查询:example.com A
	pkt := make([]byte, 0, 64)
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], 0x1234) // ID
	binary.BigEndian.PutUint16(hdr[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // QDCOUNT
	pkt = append(pkt, hdr...)
	pkt = append(pkt, 7)
	pkt = append(pkt, "example"...)
	pkt = append(pkt, 3)
	pkt = append(pkt, "com"...)
	pkt = append(pkt, 0)
	pkt = binary.BigEndian.AppendUint16(pkt, 1) // QTYPE A
	pkt = binary.BigEndian.AppendUint16(pkt, 1) // QCLASS IN

	addr, err := net.ResolveUDPAddr("udp", l.Addr)
	if err != nil {
		t.Fatal(err)
	}
	uc, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	uc.Write(pkt)
	uc.Close()

	waitFor(t, func() bool {
		conns, _ := m.Poll(l.ID)
		return len(conns) == 1
	}, 2*time.Second)
	conns, _ := m.Poll(l.ID)
	if !strings.Contains(conns[0].Data, "example.com") {
		t.Errorf("dns data=%q", conns[0].Data)
	}
}

func TestCloseAndEvents(t *testing.T) {
	sink := &recSink{}
	m := NewManager(sink, "c1", 0)
	l, err := m.Open(context.Background(), ProtoTCP, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Poll(l.ID); err == nil {
		t.Error("poll after close should error")
	}
	// 事件:open + close
	kinds := map[event.Kind]int{}
	for _, e := range sink.events {
		kinds[e.Kind]++
	}
	if kinds[event.KindListener] < 2 {
		t.Errorf("listener events=%d want >=2", kinds[event.KindListener])
	}
	if len(m.State()) != 0 {
		t.Error("state should be empty after close")
	}
}

func TestBadProto(t *testing.T) {
	m := NewManager(event.Discard, "c1", 0)
	if _, err := m.Open(context.Background(), Proto("icmp"), 0); err == nil {
		t.Error("unsupported proto should error")
	}
}

type recSink struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recSink) Emit(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
