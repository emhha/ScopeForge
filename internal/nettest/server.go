// Package nettest supplies local integration fixtures compatible with netloom's dial guard.
package nettest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func Server(t *testing.T, handler http.Handler, secure bool) *httptest.Server {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
			continue
		}
		listener, err = net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		if err == nil {
			break
		}
	}
	if listener == nil {
		t.Fatal("netloom integration requires a local non-loopback interface")
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

// Proxy is a local fixture for native upstream-proxy handling, including CONNECT.
func Proxy(t *testing.T, onRequest func(*http.Request)) *httptest.Server {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		if r.Method == http.MethodConnect {
			target, err := net.DialTimeout("tcp", r.Host, 3*time.Second)
			if err != nil {
				http.Error(w, "fixture dial failed", 502)
				return
			}
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				_ = target.Close()
				return
			}
			defer client.Close()
			defer target.Close()
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffered.Flush()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(target, buffered); _ = target.Close(); close(done) }()
			_, _ = io.Copy(client, target)
			_ = client.Close()
			<-done
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		response, err := transport.RoundTrip(request)
		if err != nil {
			http.Error(w, "fixture upstream failed", 502)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(func() { transport.CloseIdleConnections(); proxy.Close() })
	return proxy
}
