package server

import (
	"context"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/emhha/scopeforge/modelutil"

	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

// 空转回合(仅思考、无正文无工具)的识别与续跑，见 steerHooks.Stop。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ReasoningContent: text}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, ReasoningContent: "先扫端口", ToolCalls: []llm.ToolCall{
		{
			ID: "t1", Type: "function",
			Function: llm.FunctionDefinitionParam{Name: "run_nuclei", Arguments: nil},
		}},
	}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"仅思考", []llm.Message{modelutil.User("开始"), assistantThinking("想想")}, true},
		{"思考+工具", []llm.Message{modelutil.User("开始"), toolUse}, false},
		{"思考+正文", []llm.Message{assistantThinking("想想"), {
			Role: llm.RoleAssistant, Content: "结论", ReasoningContent: "x",
		}}, false},
		{"正文只有空白字符", []llm.Message{{
			Role: llm.RoleAssistant, Content: "  \n ", ReasoningContent: "x",
		}}, true},
		{"完全空的 assistant 回合", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 工具结果是 user 角色，判定必须回溯到它前面那条 assistant，而不是就近误判。
		{"最后一条是工具结果", []llm.Message{toolUse, {
			Role: llm.RoleUser,
		}}, false},
		{"没有 assistant 消息", []llm.Message{modelutil.User("开始")}, false},
		{"空历史", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks 是一个可编程的 inner HookRunner，用来验证 steerHooks 对 inner 决定的尊重。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerNativeCallbackPreventsQueuedCall(t *testing.T) {
	hooks := steerHooks{drain: func() (string, bool) { return "change direction", true }}
	result, err := hooks.callbacks().RunBeforeTool(t.Context(), &tool.BeforeToolArgs{ToolName: "probe", Arguments: []byte(`{}`)})
	if err != nil || result == nil || result.CustomResult == nil {
		t.Fatalf("steering lost: %v", err)
	}
}
