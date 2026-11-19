// Package modeltest supplies native response fixtures for offline contract tests.
package modeltest

import (
	"context"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

type functionModel struct {
	fn func(context.Context, *model.Request) (<-chan *model.Response, error)
}

func Func(fn func(context.Context, *model.Request) (<-chan *model.Response, error)) *functionModel {
	return &functionModel{fn}
}

func (f *functionModel) Info() model.Info { return model.Info{Name: "fixture"} }
func (f *functionModel) GenerateContent(ctx context.Context, r *model.Request) (<-chan *model.Response, error) {
	return f.fn(ctx, r)
}
func Responses(events ...*model.Response) <-chan *model.Response {
	out := make(chan *model.Response, len(events))
	for _, ev := range events {
		out <- ev
	}
	close(out)
	return out
}
func Text(text string) *model.Response {
	return &model.Response{Choices: []model.Choice{{Delta: model.Message{Role: model.RoleAssistant, Content: text}}}, IsPartial: true}
}
func Final(text string) *model.Response {
	stop := "stop"
	return &model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, Content: text}, FinishReason: &stop}}, Done: true}
}
func Failure(err error) *model.Response {
	return &model.Response{Error: &model.ResponseError{Type: model.ErrorTypeAPIError, Message: err.Error()}, Done: true}
}
func FromCompletion(ctx context.Context, r *model.Request, fn func(context.Context, model.Request) (model.Message, string, model.Usage, error)) (<-chan *model.Response, error) {
	m, stop, u, err := fn(ctx, *r)
	if err != nil {
		return nil, err
	}
	return Responses(&model.Response{Choices: []model.Choice{{Message: m, FinishReason: &stop}}, Usage: &u, Done: true}), nil
}
