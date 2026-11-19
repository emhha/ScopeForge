package netrequest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkhttp "github.com/emhha/netloom/nethttp"
	"github.com/emhha/scopeforge/internal/nettest"
	"github.com/emhha/scopeforge/traffic"
)

func TestNativeHTTPRedirectValidationAndSingleCapturePerHop(t *testing.T) {
	var calls atomic.Int32
	second := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization leaked across origins")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "native response")
	}), false)
	first := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("initial authentication missing")
		}
		w.Header().Set("Location", second.URL+"/final")
		w.WriteHeader(302)
	}), false)
	service, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	store, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checked := []string{}
	out, err := service.Do(t.Context(), sdkhttp.RequestInput{URL: first.URL + "/start", Headers: map[string]string{"Authorization": "Bearer fixture"}, FollowRedirects: true}, store, func(_ context.Context, u string) error { checked = append(checked, u); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != 200 || out.Body != "native response" || out.URL != second.URL+"/final" || len(out.ExchangeIDs) != 2 || len(checked) != 2 || calls.Load() != 2 {
		t.Fatalf("output=%+v checked=%v calls=%d", out, checked, calls.Load())
	}
	if count, err := store.Count(); err != nil || count != 2 {
		t.Fatalf("captured=%d err=%v", count, err)
	}
	calls.Store(0)
	_, err = service.Do(t.Context(), sdkhttp.RequestInput{URL: first.URL + "/start", Headers: map[string]string{"Authorization": "Bearer fixture"}, FollowRedirects: true}, nil, func(_ context.Context, u string) error {
		if strings.HasPrefix(u, second.URL) {
			return errors.New("scope denied")
		}
		return nil
	})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("denied redirect err=%v calls=%d", err, calls.Load())
	}
}
func TestNativeHTTPDefaultsVerifyTLS(t *testing.T) {
	target := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "tls fixture") }), true)
	service, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Do(t.Context(), sdkhttp.RequestInput{URL: target.URL}, nil, nil); err == nil {
		t.Fatal("untrusted target certificate was accepted by default")
	}
	out, err := service.Do(t.Context(), sdkhttp.RequestInput{URL: target.URL, InsecureTLS: true}, nil, nil)
	if err != nil || out.Body != "tls fixture" {
		t.Fatalf("explicit TLS override output=%+v error=%v", out, err)
	}
}
func TestNativeHTTPCloseCancelsActiveRequestWithoutReplay(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	upstream := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" {
			t.Errorf("method=%s", r.Method)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}), false)
	service, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := service.Do(context.Background(), sdkhttp.RequestInput{URL: upstream.URL + "/change", Method: "POST", Body: "once"}, nil, nil)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach fixture")
	}
	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native runtime shutdown blocked")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request outlived runtime")
	}
	if calls.Load() != 1 {
		t.Fatalf("mutating request replayed %d times", calls.Load())
	}
}

func TestNativeHTTPDecodesPreviewAndRetainsWireEvidence(t *testing.T) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	_, _ = writer.Write([]byte("compressed native response"))
	_ = writer.Close()
	target := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(encoded.Bytes())
	}), false)
	service, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	store, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	out, err := service.Do(t.Context(), sdkhttp.RequestInput{URL: target.URL}, store, nil)
	if err != nil || out.Body != "compressed native response" || out.BodyEncoding != "utf8" || len(out.ExchangeIDs) != 1 {
		t.Fatalf("output=%+v err=%v", out, err)
	}
	if err := store.ReadEvidence(t.Context(), out.ExchangeIDs, func(exchange traffic.EvidenceExchange) error {
		raw, err := io.ReadAll(exchange.Response)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, encoded.Bytes()) {
			t.Error("wire evidence was replaced with decoded preview")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
