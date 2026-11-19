package traffic

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/emhha/netloom"
	"github.com/emhha/netloom/mitm"
)

func (t *Traffic) initProxy() error {
	path := filepath.Join(t.dir, "_ca", "netloom-ca.pem")
	data, err := os.ReadFile(path)
	var authority *mitm.Authority
	if err == nil {
		var certificate, privateKey []byte
		for len(data) > 0 {
			block, rest := pem.Decode(data)
			if block == nil {
				return errors.New("invalid persisted netloom CA")
			}
			data = rest
			switch block.Type {
			case "CERTIFICATE":
				certificate = pem.EncodeToMemory(block)
			case "PRIVATE KEY":
				privateKey = pem.EncodeToMemory(block)
			}
		}
		authority, err = mitm.LoadAuthority(certificate, privateKey)
	} else if os.IsNotExist(err) {
		authority, err = mitm.NewAuthority()
		if err == nil {
			cert, key := authority.PEM()
			var f *os.File
			f, err = os.CreateTemp(filepath.Dir(path), ".netloom-ca-*")
			if err == nil {
				name := f.Name()
				defer os.Remove(name)
				_, err = f.Write(append(cert, key...))
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil {
					err = os.Rename(name, path)
				}
			}
		}
	}
	if err != nil {
		return err
	}
	if err = os.WriteFile(t.CACertPath(), authority.CertificatePEM(), 0644); err != nil {
		return err
	}
	t.engine, err = netloom.New(netloom.Config{Threads: 16, MaxConcurrentPerHost: 4, Retries: -1, Timeout: 60 * time.Second, ConnectionPool: &netloom.ConnectionPoolConfig{MaxClients: 16, IdleTimeout: 30 * time.Second}})
	if err != nil {
		return err
	}
	t.proxy = mitm.New(authority, &proxyBackend{traffic: t}, mitm.Options{})
	return nil
}
func (t *Traffic) serveProxy() error {
	t.proxyMu.Lock()
	if t.stopping() {
		t.proxyMu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", t.addr)
	if err != nil {
		t.proxyMu.Unlock()
		return err
	}
	t.addr = listener.Addr().String()
	server := &http.Server{Handler: t.proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	t.server = server
	t.proxyWG.Add(1)
	t.proxyMu.Unlock()
	defer t.proxyWG.Done()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (t *Traffic) closeProxy() {
	t.proxyMu.Lock()
	if t.proxy != nil {
		t.proxy.Close()
	}
	if t.server != nil {
		_ = t.server.Close()
	}
	t.proxyMu.Unlock()
	if t.engine != nil {
		_ = t.engine.Close()
	}
	t.proxyWG.Wait()
}

type proxyBackend struct{ traffic *Traffic }

func (b *proxyBackend) CheckTarget(ctx context.Context, u *url.URL) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.traffic.stopping() {
		return context.Canceled
	}
	_, err := netloom.HTTPURL(u.String())
	return err
}
func (b *proxyBackend) RoundTrip(req *http.Request) (*http.Response, error) {
	t := b.traffic
	t.proxyMu.Lock()
	if t.stopping() {
		t.proxyMu.Unlock()
		return nil, context.Canceled
	}
	t.proxyWG.Add(1)
	t.proxyMu.Unlock()
	owned := false
	defer func() {
		if !owned {
			t.proxyWG.Done()
		}
	}()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	headers := fhttp.Header(req.Header.Clone())
	headers.Set("Host", req.Host)
	input := &netloom.Request{Method: req.Method, URL: req.URL.String(), Header: headers, Body: body, VerifyTLS: false, Timeout: 60 * time.Second}
	if upstream := t.upstream.Load(); upstream != nil {
		input.Proxy = upstream.String()
	}
	stream, err := t.engine.SendStream(req.Context(), input)
	if err != nil {
		return nil, err
	}
	out := stream.Response
	capture, err := t.NewCapture(Capture{URL: input.URL, Method: input.Method, Proto: req.Proto, Host: req.Host, RequestHeaders: http.Header(out.RequestHeaders), RequestBody: body, ResponseHeaders: http.Header(out.Header), StatusCode: out.StatusCode})
	if err != nil {
		_ = stream.Body.Close()
		return nil, err
	}
	owned = true
	wrapped := &captureBody{body: stream.Body, reader: io.TeeReader(stream.Body, capture), finish: func() {
		defer t.proxyWG.Done()
		_, err := capture.Finish(stream.Status().Complete || req.Method == http.MethodHead)
		if err != nil {
			log.Printf("[traffic] capture failed: %v", err)
		}
	}}
	return &http.Response{StatusCode: out.StatusCode, Header: http.Header(out.Header).Clone(), Body: wrapped, ContentLength: out.ContentLength, Request: req, Proto: out.HTTPVersion}, nil
}

type captureBody struct {
	body   io.ReadCloser
	reader io.Reader
	finish func()
	once   sync.Once
	err    error
}

func (b *captureBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *captureBody) Close() error {
	b.once.Do(func() { b.err = b.body.Close(); b.finish() })
	return b.err
}
