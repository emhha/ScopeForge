package agent

import (
	"context"

	knowledgetool "trpc.group/trpc-go/trpc-agent-go/knowledge/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// Preserve native retrieval, schema and results while treating a search miss as
// a normal result. Retrieval failures remain errors.
type knowledgeSearchTool struct{ tool.CallableTool }

func (t knowledgeSearchTool) Call(ctx context.Context, input []byte) (any, error) {
	result, err := t.CallableTool.Call(ctx, input)
	if err != nil && err.Error() == "no relevant information found" {
		return &knowledgetool.KnowledgeSearchResponse{Documents: []*knowledgetool.DocumentResult{}, Message: "未找到相关知识。可根据任务现有证据继续；没有新的检索线索时无需重复搜索。"}, nil
	}
	return result, err
}
