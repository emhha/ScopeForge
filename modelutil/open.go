package modelutil

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	openaisdk "github.com/openai/openai-go"
	openaioption "github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/v3/option"
	"trpc.group/trpc-go/trpc-agent-go/model"
	nativeanthropic "trpc.group/trpc-go/trpc-agent-go/model/anthropic"
	nativeopenai "trpc.group/trpc-go/trpc-agent-go/model/openai"
	nativeresponses "trpc.group/trpc-go/trpc-agent-go/model/openai/sdkv3/responses"
)

// Open builds a native provider adapter with the product's transport and token
// field settings. A nil client uses a direct transport without environment proxies.
func Open(format, modelName, baseURL, apiKey string, maxTokens int, maxTokensField string, client *http.Client) (model.Model, error) {
	if strings.TrimSpace(modelName) == "" || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("model and api key are required")
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client = &http.Client{Transport: transport}
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai", "chat", "chat_completions", "chat-completions":
		field := strings.TrimSpace(maxTokensField)
		if field == "" {
			field = "max_tokens"
		}
		if field != "max_tokens" && field != "max_completion_tokens" {
			return nil, fmt.Errorf("unsupported Chat token field %q", field)
		}
		opts := []nativeopenai.Option{
			// Preserve the product's explicit selector using the SDK request hook;
			// native adapters still own every encoder, decoder and stream parser.
			nativeopenai.WithChatRequestCallback(func(_ context.Context, req *openaisdk.ChatCompletionNewParams) {
				if field == "max_tokens" {
					if req.MaxCompletionTokens.Valid() {
						req.MaxTokens = req.MaxCompletionTokens
					}
					req.MaxCompletionTokens = param.Opt[int64]{}
				} else {
					if req.MaxTokens.Valid() {
						req.MaxCompletionTokens = req.MaxTokens
					}
					req.MaxTokens = param.Opt[int64]{}
				}
			}),
			nativeopenai.WithAPIKey(apiKey),
			nativeopenai.WithOpenAIOptions(openaioption.WithHTTPClient(client), openaioption.WithMaxRetries(0)),
		}
		if baseURL != "" {
			opts = append(opts, nativeopenai.WithBaseURL(baseURL))
		}
		return nativeopenai.New(modelName, opts...), nil
	case "anthropic":
		opts := []nativeanthropic.Option{
			nativeanthropic.WithChatRequestCallback(applyAnthropicGeneration),
			nativeanthropic.WithAPIKey(apiKey),
			nativeanthropic.WithAnthropicClientOptions(anthropicoption.WithHTTPClient(client), anthropicoption.WithMaxRetries(0), anthropicoption.WithMiddleware(applyAnthropicHeaders)),
		}
		if baseURL != "" {
			opts = append(opts, nativeanthropic.WithBaseURL(baseURL))
		}
		return nativeanthropic.New(modelName, opts...), nil
	case "responses", "openai-responses", "openai_responses":
		opts := []nativeresponses.Option{
			nativeresponses.WithAPIKey(apiKey),
			nativeresponses.WithOpenAIOptions(option.WithHTTPClient(client), option.WithMaxRetries(0)),
		}
		if baseURL != "" {
			opts = append(opts, nativeresponses.WithBaseURL(baseURL))
		}
		return nativeresponses.New(modelName, opts...), nil
	default:
		return nil, fmt.Errorf("format %q", format)
	}
}
