package modelutil

import (
	"context"
	"net/http"

	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"trpc.group/trpc-go/trpc-agent-go/model"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
)

type anthropicGenerationKey struct{}
type anthropicGeneration struct {
	thinking, effort string
	headers          map[string]string
	extra            map[string]any
}

// WithAnthropicRequest carries explicit product settings to SDK hooks. The
// pinned adapter ignores Request.Headers/ExtraFields and couples effort to its
// adaptive-model allowlist. No history or executable tool is retained here.
func WithAnthropicRequest(ctx context.Context, req *model.Request) context.Context {
	config := anthropicGeneration{extra: req.ExtraFields, headers: make(map[string]string, len(req.Headers))}
	if thinking, ok := req.ExtraFields["thinking"].(map[string]string); ok {
		config.thinking = thinking["type"]
	}
	if req.ReasoningEffort != nil {
		config.effort = *req.ReasoningEffort
	}
	for key, val := range req.Headers {
		config.headers[key] = val
	}
	return context.WithValue(ctx, anthropicGenerationKey{}, config)
}

func applyAnthropicHeaders(req *http.Request, next anthropicoption.MiddlewareNext) (*http.Response, error) {
	if config, ok := req.Context().Value(anthropicGenerationKey{}).(anthropicGeneration); ok {
		for key, val := range config.headers {
			req.Header.Set(key, val)
		}
	}
	return next(req)
}

func applyAnthropicGeneration(ctx context.Context, req *anthropicsdk.MessageNewParams) {
	config, ok := ctx.Value(anthropicGenerationKey{}).(anthropicGeneration)
	if !ok {
		return
	}
	if tools, ok := config.extra["tools"]; ok {
		fields := req.ExtraFields()
		if fields == nil {
			fields = map[string]any{}
		}
		fields["tools"] = tools
		req.SetExtraFields(fields)
	}
	if config.thinking != "" {
		fields := make(map[string]any)
		for key, val := range req.ExtraFields() {
			fields[key] = val
		}
		fields["thinking"] = map[string]string{"type": config.thinking}
		req.SetExtraFields(fields)
	}
	if config.effort != "" {
		req.OutputConfig.Effort = anthropicsdk.OutputConfigEffort(config.effort)
	}
}
