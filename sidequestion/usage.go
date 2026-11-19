package sidequestion

import "trpc.group/trpc-go/trpc-agent-go/model"

// Usage is the persisted/frontend DTO; execution uses the native counters.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

func StoredUsage(native model.Usage) Usage {
	return Usage{InputTokens: native.PromptTokens, OutputTokens: native.CompletionTokens, CacheReadTokens: max(native.PromptTokensDetails.CachedTokens, native.PromptTokensDetails.CacheReadTokens), CacheWriteTokens: native.PromptTokensDetails.CacheCreationTokens}
}
func mergeUsage(total *model.Usage, next model.Usage) {
	total.PromptTokens = max(total.PromptTokens, next.PromptTokens)
	total.CompletionTokens = max(total.CompletionTokens, next.CompletionTokens)
	total.TotalTokens = max(total.TotalTokens, next.TotalTokens)
	total.PromptTokensDetails.CachedTokens = max(total.PromptTokensDetails.CachedTokens, next.PromptTokensDetails.CachedTokens, next.PromptTokensDetails.CacheReadTokens)
	total.PromptTokensDetails.CacheReadTokens = total.PromptTokensDetails.CachedTokens
	total.PromptTokensDetails.CacheCreationTokens = max(total.PromptTokensDetails.CacheCreationTokens, next.PromptTokensDetails.CacheCreationTokens)
}
func addUsage(total *model.Usage, next model.Usage) {
	total.PromptTokens += next.PromptTokens
	total.CompletionTokens += next.CompletionTokens
	total.TotalTokens += next.TotalTokens
	total.PromptTokensDetails.CachedTokens += max(next.PromptTokensDetails.CachedTokens, next.PromptTokensDetails.CacheReadTokens)
	total.PromptTokensDetails.CacheReadTokens = total.PromptTokensDetails.CachedTokens
	total.PromptTokensDetails.CacheCreationTokens += next.PromptTokensDetails.CacheCreationTokens
}
