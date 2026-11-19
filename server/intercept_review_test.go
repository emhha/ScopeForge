package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/modeltest"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

type reviewCaptureProvider struct{ request llm.Request }

func (p *reviewCaptureProvider) Info() llm.Info { return llm.Info{Name: "review"} }
func (p *reviewCaptureProvider) GenerateContent(_ context.Context, request *llm.Request) (<-chan *llm.Response, error) {
	p.request = *request
	return modeltest.Responses(modeltest.Final(`{"decision":"ask","comment":"实际操作：删除文件；成功后的后果：文件会丢失，归属尚未确认；命中规则：ASK（归属不明）"}`)), nil
}

// Assert the actual model request, rather than merely the envelope helper.
func TestReviewCompletionSendsCurrentCallWithoutHistory(t *testing.T) {
	ctx := intercept.WithReviewContext(t.Context(), "/tmp/review-fixture", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: "清理本次测试文件"})
	ctx, trace := intercept.WithTrace(ctx, "GLOBAL_OVERVIEW_SENTINEL", []db.InterceptContextEntry{
		{Kind: "tool_use", Tool: "Write", ToolUseID: "created", Text: `{"path":"probe.txt"}`},
		{Kind: "tool_result", ToolUseID: "created", Text: "file created"},
		{Kind: "assistant", Text: "untrusted worker speculation"},
	})
	args := json.RawMessage(`{"command":"rm probe.txt","timeout":1000}`)
	trace.Start("current", "Bash", args)
	input, err := intercept.BuildReviewInput(intercept.WithCall(ctx, "Bash", args), "Bash", args)
	if err != nil {
		t.Fatal(err)
	}
	p := &reviewCaptureProvider{}
	prompt := intercept.EffectiveJudgePrompt(intercept.DefaultJudgePrompt)
	reply, err := reviewCompletion(ctx, p, prompt, input)
	if err != nil || intercept.ParseVerdict(reply).Action != "ask" {
		t.Fatalf("%s %v", reply, err)
	}
	if len(p.request.Messages) != 2 || p.request.Messages[0].Content != prompt {
		t.Fatal("wrong review request roles")
	}
	if !strings.Contains(prompt, intercept.JudgeOutputContract) || p.request.MaxTokens == nil || *p.request.MaxTokens < 1024 {
		t.Fatal("review request cannot return a complete explanation")
	}
	body := p.request.Messages[1].Content
	var got intercept.ReviewInput
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("unexpected envelope keys: %s", body)
	}
	for _, key := range []string{"version", "working_directory", "background", "tool_name", "arguments"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if strings.Contains(body, "file created") || strings.Contains(body, `"path"`) {
		t.Fatal("history leaked into actual model request")
	}
	if got.Version != 4 || got.Background == nil || got.Background.Source != intercept.BackgroundUserMessage || got.Background.Text != "清理本次测试文件" || got.WorkingDir != "/tmp/review-fixture" || string(got.Arguments) != string(args) || strings.Contains(body, "worker speculation") || strings.Contains(body, "GLOBAL_OVERVIEW_SENTINEL") {
		t.Fatalf("wrong model input: %s", body)
	}
}
