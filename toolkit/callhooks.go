package toolkit

import (
	"context"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type callHooksKey struct{}

func WithCallHooks(ctx context.Context, hooks *tool.Callbacks) context.Context {
	return context.WithValue(ctx, callHooksKey{}, hooks)
}
func CallHooksFrom(ctx context.Context) *tool.Callbacks {
	v, _ := ctx.Value(callHooksKey{}).(*tool.Callbacks)
	return v
}
