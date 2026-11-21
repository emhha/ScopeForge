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
	headers map[string]string
	extra   map[string]any
}

// WithAnthropicRequest carries explicit headers and tool declarations to native
// request hooks. Thinking and effort use the SDK's typed request fields directly.
func WithAnthropicRequest(ctx context.Context, req *model.Request) context.Context {
	config := anthropicGeneration{extra: req.ExtraFields, headers: make(map[string]string, len(req.Headers))}
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
}
