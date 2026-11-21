package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const modelCatalogURL = "https://models.dev/experimental/v2.0/api.json"

// The live catalog is over 12 MB before compression. The body download needs a
// larger budget than establishing a connection; the form remains editable.
const modelCatalogTimeout = 120 * time.Second

type catalogReasoning struct {
	Supported bool            `json:"supported"`
	Toggle    bool            `json:"toggle"`
	Effort    []string        `json:"effort"`
	Budget    json.RawMessage `json:"budget,omitempty"`
}
type catalogAPI struct {
	Format         string           `json:"format"`
	BaseURL        string           `json:"base_url"`
	MaxTokensField string           `json:"max_tokens_field,omitempty"`
	Reasoning      catalogReasoning `json:"reasoning"`
}
type catalogModel struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Context int          `json:"context"`
	Output  int          `json:"output"`
	APIs    []catalogAPI `json:"apis"`
}
type catalogProvider struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Models []catalogModel `json:"models"`
}
type modelCatalog struct {
	Source    string            `json:"source"`
	Origin    string            `json:"origin"`
	UpdatedAt time.Time         `json:"updated_at"`
	Providers []catalogProvider `json:"providers"`
	Stale     bool              `json:"stale,omitempty"`
	Warning   string            `json:"warning,omitempty"`
}

// Only normalize protocols supported by our native tRPC adapters. Do not offer
// image/audio/embedding models or advertise a provider's unsupported native API.
func parseModelCatalog(data []byte) ([]catalogProvider, error) {
	type capabilities struct {
		Tools struct {
			Supported *bool `json:"supported"`
		} `json:"tools"`
		Reasoning catalogReasoning `json:"reasoning"`
	}
	var source map[string]struct {
		Name   string `json:"name"`
		Models map[string]struct {
			ID           string       `json:"id"`
			Name         string       `json:"name"`
			Type         string       `json:"type"`
			Capabilities capabilities `json:"capabilities"`
			Limit        struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Recommended string `json:"recommended_api"`
			API         map[string]struct {
				BaseURL       string          `json:"base_url"`
				Capabilities  json.RawMessage `json:"capabilities"`
				Compatibility struct {
					MaxTokensField string `json:"max_tokens_field"`
				} `json:"compatibility"`
			} `json:"api"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	providers := []catalogProvider{}
	formats := map[string]string{"messages": "anthropic", "responses": "openai-responses", "chat-completions": "openai"}
	for id, p := range source {
		provider := catalogProvider{ID: id, Name: p.Name, Models: []catalogModel{}}
		for key, m := range p.Models {
			if m.Type != "chat" || m.Limit.Context <= 0 || m.Capabilities.Tools.Supported == nil || !*m.Capabilities.Tools.Supported {
				continue
			}
			model := catalogModel{ID: m.ID, Name: m.Name, Context: m.Limit.Context, Output: m.Limit.Output, APIs: []catalogAPI{}}
			if model.ID == "" {
				model.ID = key
			}
			order := []string{m.Recommended, "responses", "messages", "chat-completions"}
			seen := map[string]bool{}
			for _, name := range order {
				endpoint, ok := m.API[name]
				format := formats[name]
				if !ok || format == "" || seen[name] {
					continue
				}
				seen[name] = true
				u, err := url.Parse(endpoint.BaseURL)
				if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
					continue
				}
				var caps capabilities
				encoded, _ := json.Marshal(m.Capabilities)
				if json.Unmarshal(encoded, &caps) != nil {
					continue
				}
				if len(endpoint.Capabilities) > 0 {
					if json.Unmarshal(endpoint.Capabilities, &caps) != nil {
						continue
					}
				}
				if caps.Tools.Supported != nil && !*caps.Tools.Supported {
					continue
				}
				base := strings.TrimRight(endpoint.BaseURL, "/")
				if format == "anthropic" {
					base = strings.TrimSuffix(base, "/v1")
				} // SDK appends v1/messages itself
				field := endpoint.Compatibility.MaxTokensField
				if format == "openai" && id == "openai" && caps.Reasoning.Supported && field == "" {
					field = "max_completion_tokens"
				}
				if field != "max_completion_tokens" {
					field = ""
				}
				model.APIs = append(model.APIs, catalogAPI{Format: format, BaseURL: base, MaxTokensField: field, Reasoning: caps.Reasoning})
			}
			if len(model.APIs) > 0 {
				provider.Models = append(provider.Models, model)
			}
		}
		if len(provider.Models) > 0 {
			sort.Slice(provider.Models, func(i, j int) bool { return provider.Models[i].ID < provider.Models[j].ID })
			providers = append(providers, provider)
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	if len(providers) == 0 {
		return nil, errors.New("模型目录未包含受支持的对话模型")
	}
	return providers, nil
}

func fetchModelCatalog(ctx context.Context, client *http.Client, source string) (*modelCatalog, error) {
	for attempt := 0; ; attempt++ {
		catalog, err := fetchModelCatalogOnce(ctx, client, source)
		var networkErr net.Error
		if attempt == 0 && ctx.Err() == nil && errors.As(err, &networkErr) && networkErr.Timeout() {
			continue
		}
		return catalog, err
	}
}

func fetchModelCatalogOnce(ctx context.Context, client *http.Client, source string) (*modelCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ScopeForge/model-catalog")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录服务返回 HTTP %d", resp.StatusCode)
	}
	const maxBytes = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, errors.New("模型目录超过 32 MiB")
	}
	providers, err := parseModelCatalog(data)
	if err != nil {
		return nil, err
	}
	return &modelCatalog{Source: modelCatalogURL, UpdatedAt: time.Now().UTC(), Origin: "online", Providers: providers}, nil
}

// Public catalog requests honor the server environment proxy independently of
// target traffic. An explicitly configured application proxy takes precedence.
func modelCatalogTransport(proxy string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSHandshakeTimeout = 20 * time.Second
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("全局出口代理无效")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return transport, nil
}

func modelCatalogError(err error) string {
	var networkErr net.Error
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("模型目录下载超时（%d 秒），请重试并检查后端网络连接", int(modelCatalogTimeout/time.Second))
	}
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "连接模型目录超时：" + err.Error()
	}
	return "获取模型目录失败：" + err.Error()
}

func (s *Server) llmModelCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("refresh") != "true" {
		catalog, err := bundledModelCatalog()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, catalog)
		return
	}
	transport, err := modelCatalogTransport(s.m.GlobalProxy())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(r.Context(), modelCatalogTimeout)
	defer cancel()
	catalog, err := fetchModelCatalog(ctx, &http.Client{Transport: transport}, modelCatalogURL)
	if err != nil {
		writeErr(w, 502, modelCatalogError(err))
		return
	}
	writeJSON(w, 200, catalog)
}
