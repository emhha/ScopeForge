// Package sessionctx carries product call identity independently of session storage.
package sessionctx

import "context"

type key struct{}

func WithID(ctx context.Context, id string) context.Context { return context.WithValue(ctx, key{}, id) }
func ID(ctx context.Context) string                         { id, _ := ctx.Value(key{}).(string); return id }
