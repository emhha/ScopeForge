package agent

import (
	"context"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func (t *turn) callbacks() *tool.Callbacks {
	callbacks := tool.NewCallbacks().RegisterBeforeTool(func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
		if sources, _ := ctx.Value(nodeSourcesKey{}).(*nodeSources); sources != nil {
			return &tool.BeforeToolResult{Context: sources.begin(ctx, args.ToolCallID)}, nil
		}
		return nil, nil
	}).RegisterBeforeTool(func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
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
			if extra.ToolResultMessages != nil {
				previous, next := callbacks.ToolResultMessages, extra.ToolResultMessages
				callbacks.RegisterToolResultMessages(func(ctx context.Context, in *tool.ToolResultMessagesInput) (any, error) {
					if previous != nil {
						messages, err := previous(ctx, in)
						if list, ok := messages.([]model.Message); ok && len(list) == 0 {
							messages = nil
						}
						if messages != nil || err != nil {
							return messages, err
						}
					}
					return next(ctx, in)
				})
			}
		}
	}
	// The pinned SDK carries context between callbacks internally but returns the
	// last result without merging earlier context. Preserve it when a later hook
	// only modifies arguments, so execution still owns its provenance batch.
	for i, callback := range callbacks.BeforeTool {
		callbacks.BeforeTool[i] = func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
			result, err := callback(ctx, args)
			if result != nil && result.Context == nil {
				copy := *result
				copy.Context = ctx
				result = &copy
			}
			return result, err
		}
	}
	return callbacks
}
