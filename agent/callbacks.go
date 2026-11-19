package agent

import (
	"context"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func (t *turn) callbacks() *tool.Callbacks {
	callbacks := tool.NewCallbacks().RegisterBeforeTool(func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
		if args.Declaration == nil {
			return nil, nil
		}
		input := injectDefaults(args.Arguments, toolkit.SchemaMap(args.Declaration.InputSchema))
		if len(input) == 0 {
			input = []byte(`{}`)
		}
		if err := toolkit.Validate(args.Declaration.InputSchema, input); err != nil {
			return &tool.BeforeToolResult{CustomResult: toolkit.Error("工具参数无效：" + err.Error()), SkipStateDelta: true}, nil
		}
		return &tool.BeforeToolResult{ModifiedArguments: input}, nil
	})
	for _, extra := range []*tool.Callbacks{t.config.Hooks, t.config.AugmentCallbacks} {
		if extra != nil {
			callbacks.BeforeTool = append(callbacks.BeforeTool, extra.BeforeTool...)
			callbacks.AfterTool = append(callbacks.AfterTool, extra.AfterTool...)
		}
	}
	return callbacks
}
