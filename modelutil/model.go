// Package modelutil contains product policies over native tRPC model calls.
package modelutil

import (
	"context"
	"encoding/json"
	"errors"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

func User(text string) model.Message { return model.Message{Role: model.RoleUser, Content: text} }
func MergeUsage(dst *model.Usage, src model.Usage) {
	dst.PromptTokens = max(dst.PromptTokens, src.PromptTokens)
	dst.CompletionTokens = max(dst.CompletionTokens, src.CompletionTokens)
	dst.TotalTokens = max(dst.TotalTokens, src.TotalTokens)
	dst.PromptTokensDetails.CachedTokens = max(dst.PromptTokensDetails.CachedTokens, src.PromptTokensDetails.CachedTokens, src.PromptTokensDetails.CacheReadTokens)
	dst.PromptTokensDetails.CacheCreationTokens = max(dst.PromptTokensDetails.CacheCreationTokens, src.PromptTokensDetails.CacheCreationTokens)
}
func AddUsage(dst *model.Usage, src model.Usage) {
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.TotalTokens += src.TotalTokens
	dst.PromptTokensDetails.CachedTokens += max(src.PromptTokensDetails.CachedTokens, src.PromptTokensDetails.CacheReadTokens)
	dst.PromptTokensDetails.CacheCreationTokens += src.PromptTokensDetails.CacheCreationTokens
}
func Counts(u model.Usage) (int, int, int, int) {
	return u.PromptTokens, u.CompletionTokens, max(u.PromptTokensDetails.CachedTokens, u.PromptTokensDetails.CacheReadTokens), u.PromptTokensDetails.CacheCreationTokens
}
func Complete(ctx context.Context, m model.Model, req model.Request) (message model.Message, stop string, usage model.Usage, err error) {
	req.Stream = false
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := m.GenerateContent(callCtx, &req)
	if err != nil {
		return message, stop, usage, err
	}
	if ch == nil {
		return message, stop, usage, errors.New("model returned nil channel")
	}
	seen := false
	for ev := range ch {
		if ev == nil {
			continue
		}
		if ev.Usage != nil {
			MergeUsage(&usage, *ev.Usage)
		}
		if ev.Error != nil {
			err = ev.Error
			continue
		}
		if !ev.IsPartial && len(ev.Choices) > 0 {
			message = ev.Choices[0].Message
			seen = true
			if ev.Choices[0].FinishReason != nil {
				stop = *ev.Choices[0].FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && !seen {
		err = errors.New("model returned no completion")
	}
	return
}
func Estimate(req model.Request) int {
	b, _ := json.Marshal(req.Messages)
	return len(b)/3 + len(req.Tools)*120 + 32
}
