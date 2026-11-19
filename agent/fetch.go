package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Autumn-27/artex/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func fetchTool(proxy, ca string) tool.CallableTool {
	return toolkit.New("WebFetch", "Fetch an HTTP(S) URL through the configured recording proxy.", map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []string{"url"}}, func(ctx context.Context, args json.RawMessage) (toolkit.Output, error) {
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return toolkit.Output{}, err
		}
		u, err := url.Parse(in.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return toolkit.Output{}, fmt.Errorf("HTTP(S) URL is required")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		defer transport.CloseIdleConnections()
		if proxy != "" {
			if !strings.Contains(proxy, "://") {
				proxy = "http://" + proxy
			}
			p, err := url.Parse(proxy)
			if err != nil {
				return toolkit.Output{}, err
			}
			transport.Proxy = http.ProxyURL(p)
		}
		if ca != "" {
			data, err := os.ReadFile(ca)
			if err != nil {
				return toolkit.Output{}, err
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(data) {
				return toolkit.Output{}, fmt.Errorf("invalid proxy CA")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
		client := &http.Client{Transport: transport, Timeout: 60 * time.Second}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return toolkit.Output{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return toolkit.Output{}, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		if err != nil {
			return toolkit.Output{}, err
		}
		body, _ := json.Marshal(map[string]any{"url": resp.Request.URL.String(), "status": resp.StatusCode, "body": string(data)})
		return toolkit.Output{Text: toolkit.Capture(ctx, string(body)), IsError: resp.StatusCode >= 400}, nil
	})
}
