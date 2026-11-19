package sidequestion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

func nativeCode(code string) *string { return &code }

func TestNativeContextRecoveryRequiresExactCode(t *testing.T) {
	for _, code := range []string{"context_length_exceeded", "context_window_exceeded"} {
		if !isContextOverflow(&model.ResponseError{Code: nativeCode(code)}) {
			t.Errorf("code not recognized: %s", code)
		}
	}
	for _, err := range []error{nil, errors.New("context_length_exceeded"), &model.ResponseError{Message: "maximum context length; context_length_exceeded"}, &model.ResponseError{Type: "context_length_exceeded"}, &model.ResponseError{Code: nativeCode("gateway_context_length_exceeded")}} {
		if isContextOverflow(err) {
			t.Errorf("unsafe overflow recovery from %v", err)
		}
	}
}

type nativeModelFunc func(context.Context, *model.Request) (<-chan *model.Response, error)

func (nativeModelFunc) Info() model.Info { return model.Info{Name: "native fixture"} }
func (f nativeModelFunc) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	return f(ctx, req)
}

func TestNativeAnswerDrainsLateUsageBeforeLedger(t *testing.T) {
	for _, startupFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "startup-error-with-source"}[startupFailure], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			joined := make(chan struct{})
			mdl := nativeModelFunc(func(attempt context.Context, req *model.Request) (<-chan *model.Response, error) {
				if len(req.Tools) != 0 {
					t.Error("side native request has executable tools")
				}
				out := make(chan *model.Response)
				go func() {
					defer close(out)
					if !startupFailure {
						out <- partial("accepted partial")
						cancel()
					}
					<-attempt.Done()
					out <- &model.Response{Usage: &model.Usage{PromptTokens: 17, CompletionTokens: 4, PromptTokensDetails: model.PromptTokensDetails{CachedTokens: 5, CacheReadTokens: 5, CacheCreationTokens: 3}}}
					close(joined)
				}()
				if startupFailure {
					return out, errors.New("startup fixture")
				}
				return out, nil
			})
			calls := 0
			svc := SideQuestionService{Model: mdl, OnCall: func(out Answer, _ time.Duration, err error) {
				calls++
				select {
				case <-joined:
				default:
					t.Error("usage recorded before accepted producer joined")
				}
				usage := StoredUsage(out.Usage)
				if err == nil || usage.InputTokens != 17 || usage.OutputTokens != 4 || usage.CacheReadTokens != 5 || usage.CacheWriteTokens != 3 {
					t.Errorf("late ledger: %+v %v", out, err)
				}
			}}
			out, err := svc.Answer(ctx, fixture(), true, nil)
			if err == nil || calls != 1 || out.Usage.PromptTokens != 17 {
				t.Fatalf("answer=%+v calls=%d err=%v", out, calls, err)
			}
		})
	}
}

func TestNativeAnswerDoesNotDuplicateFinalAccumulation(t *testing.T) {
	mdl := nativeModelFunc(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		out := make(chan *model.Response, 4)
		out <- partial("hello ")
		out <- partial("world")
		out <- completed("hello world")
		out <- &model.Response{Usage: &model.Usage{PromptTokens: 8}}
		close(out)
		return out, nil
	})
	answer, err := (SideQuestionService{Model: mdl}).Answer(t.Context(), fixture(), true, nil)
	if err != nil || answer.Text != "hello world" || answer.Usage.PromptTokens != 8 {
		t.Fatalf("native accumulation: %+v %v", answer, err)
	}
	data, err := CloneRequest(fixture())
	if err != nil || len(data.Tools) != 0 {
		t.Fatal("checkpoint retained executable tools")
	}
	if !strings.Contains(data.Messages[4].Content, "verified asset") {
		t.Fatal("checkpoint discarded native tool evidence")
	}
}
