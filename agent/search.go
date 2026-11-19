package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/modelutil"
	"github.com/Autumn-27/artex/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/duckduckgo"
)

func ProbeSearch(ctx context.Context, opts WebSearchOpts, query string, limit int) ([]any, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit = max(1, min(limit, 20))
	client, err := quotaAwareHTTPClient(opts.Proxy, "")
	if err != nil {
		return nil, err
	}
	backend := strings.ToLower(strings.TrimSpace(opts.Backend))
	switch backend {
	case "", "ddgs", "duckduckgo":
		t := duckduckgo.NewTool(duckduckgo.WithHTTPClient(client))
		args, _ := json.Marshal(map[string]any{"query": query})
		result, err := t.Call(ctx, args)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(result)
		var response struct {
			Results []any `json:"results"`
		}
		if err = json.Unmarshal(b, &response); err != nil {
			return nil, err
		}
		return response.Results[:min(limit, len(response.Results))], nil
	case "brave", "brave-free":
		if opts.BraveKey == "" {
			return nil, fmt.Errorf("Brave API key is required")
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.search.brave.com/res/v1/web/search?q="+url.QueryEscape(query)+fmt.Sprintf("&count=%d", limit), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Subscription-Token", opts.BraveKey)
		response, err := searchJSON(client, req)
		if err != nil {
			return nil, err
		}
		web, _ := response["web"].(map[string]any)
		items, _ := web["results"].([]any)
		return items, nil
	case "tavily":
		if opts.TavilyKey == "" {
			return nil, fmt.Errorf("Tavily API key is required")
		}
		body, _ := json.Marshal(map[string]any{"query": query, "max_results": limit})
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+opts.TavilyKey)
		req.Header.Set("Content-Type", "application/json")
		response, err := searchJSON(client, req)
		if err != nil {
			return nil, err
		}
		items, _ := response["results"].([]any)
		return items, nil
	case "deepseek":
		cfg := Config{Format: "anthropic", BaseURL: opts.DeepSeekBaseURL, APIKey: opts.DeepSeekAPIKey, Model: opts.DeepSeekModel, Proxy: opts.Proxy, ThinkingType: "disabled"}
		m, err := cfg.NewModel()
		if err != nil {
			return nil, err
		}
		tokens := 4096
		req := model.Request{Messages: []model.Message{modelutil.User(fmt.Sprintf("Search the public web for %q. Return at most %d results with title, URL and a concise description. Use the web search tool before answering.", query, limit))}, GenerationConfig: model.GenerationConfig{MaxTokens: &tokens}, ExtraFields: map[string]any{"tools": []map[string]any{{"type": "deepseek_web_search", "name": "deepseek_web_search"}}}}
		message, _, _, err := modelutil.Complete(ctx, m, req)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(message.Content) == "" {
			return nil, fmt.Errorf("DeepSeek search returned no result")
		}
		return []any{map[string]string{"summary": message.Content}}, nil
	default:
		return nil, fmt.Errorf("unsupported search backend %q", opts.Backend)
	}
}
func searchJSON(client *http.Client, req *http.Request) (map[string]any, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search status %d", resp.StatusCode)
	}
	var result map[string]any
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result)
	return result, err
}
func searchTool(opts WebSearchOpts) tool.CallableTool {
	return toolkit.New("web_search", "Search public web results", map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}, func(ctx context.Context, args json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return toolkit.Output{}, err
		}
		res, err := ProbeSearch(ctx, opts, in.Query, 5)
		if err != nil {
			return toolkit.Output{}, err
		}
		b, _ := json.Marshal(res)
		return toolkit.Text(toolkit.Capture(ctx, string(b))), nil
	})
}
