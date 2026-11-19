package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/internal/nettest"
)

func startNetloomProxy(t *testing.T) *Traffic {
	t.Helper()
	tr, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tr.Start() }()
	for n := 0; n < 200; n++ {
		u, _ := url.Parse(tr.ProxyAddr())
		if u.Port() != "0" {
			t.Cleanup(func() {
				_ = tr.Close()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			return tr
		}
		select {
		case err := <-done:
			_ = tr.Close()
			t.Fatalf("proxy startup: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = tr.Close()
	t.Fatal("proxy did not start")
	return nil
}
func TestNetloomMITMHTTPAndReusableHTTPS(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "captured:"+r.Method+":"+r.URL.Path)
	})
	plain := nettest.Server(t, handler, false)
	secure := nettest.Server(t, handler, true)
	tr := startNetloomProxy(t)
	pem, err := os.ReadFile(tr.CACertPath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("invalid native MITM CA")
	}
	proxy, _ := url.Parse(tr.ProxyAddr())
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for _, raw := range []string{plain.URL + "/plain", secure.URL + "/tls-one", secure.URL + "/tls-two"} {
		response, err := client.Get(raw)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(raw)
		if string(data) != "captured:GET:"+u.Path {
			t.Fatalf("response=%q", data)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		count, err := tr.Count()
		if err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured=%d want 3", count)
		}
		time.Sleep(5 * time.Millisecond)
	}
	records, err := tr.Search("", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		complete, err := tr.CaptureComplete(record.ID)
		if err != nil || complete == nil || !*complete {
			t.Fatalf("capture %s incomplete: %v %v", record.ID, complete, err)
		}
	}
}
func TestNativeAuthorityPersistsAndProxyStopsBeforeStart(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(tr.CACertPath())
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	tr, err = Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	current, err := os.ReadFile(tr.CACertPath())
	if strings.Contains(string(current), "PRIVATE KEY") {
		t.Fatal("public CA includes a private key")
	}
	if err != nil || string(current) != string(original) {
		t.Fatalf("native CA changed on restart: %v", err)
	}
}
func TestIncompleteCaptureCannotBecomeCompleteEvidence(t *testing.T) {
	tr, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	id, err := tr.Record(Capture{URL: "http://fixture.test/partial", Method: "GET", Proto: "HTTP/1.1", StatusCode: 200, ResponseBody: []byte("partial"), Complete: false})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := tr.CaptureComplete(id)
	if err != nil || complete == nil || *complete {
		t.Fatalf("completeness=%v err=%v", complete, err)
	}
	if err := tr.ReadEvidence(context.Background(), []string{id}, func(EvidenceExchange) error { return nil }); err == nil {
		t.Fatal("incomplete capture was accepted as full evidence")
	}
}

func TestNetloomMITMUsesExplicitUpstreamAndCanClearIt(t *testing.T) {
	target := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "upstream fixture") }), false)
	var viaProxy atomic.Int32
	upstream := nettest.Proxy(t, func(*http.Request) { viaProxy.Add(1) })
	tr := startNetloomProxy(t)
	if err := tr.SetUpstreamProxy(upstream.URL); err != nil {
		t.Fatal(err)
	}
	proxy, _ := url.Parse(tr.ProxyAddr())
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	read := func() {
		response, err := client.Get(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	read()
	if viaProxy.Load() == 0 {
		t.Fatal("configured upstream was bypassed")
	}
	before := viaProxy.Load()
	if err := tr.SetUpstreamProxy(""); err != nil {
		t.Fatal(err)
	}
	read()
	if viaProxy.Load() != before {
		t.Fatal("clearing upstream did not restore direct dialing")
	}
}
