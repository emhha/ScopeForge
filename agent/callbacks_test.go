package agent

import (
	"context"
	"errors"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestToolResultMessageCallbacksPreserveHooksAndAugmentation(t *testing.T) {
	sentinel := errors.New("hook failed")
	for _, tc := range []struct {
		name        string
		value       any
		err         error
		wantAugment bool
	}{
		{name: "nil falls through", wantAugment: true},
		{name: "empty slice falls through", value: []model.Message{}, wantAugment: true},
		{name: "hook owns custom message", value: model.Message{Role: model.RoleTool, ToolID: "1", Content: "hook"}},
		{name: "hook error stops formatting", err: sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			augmented := false
			hooks := tool.NewCallbacks().RegisterToolResultMessages(func(context.Context, *tool.ToolResultMessagesInput) (any, error) {
				return tc.value, tc.err
			})
			augment := tool.NewCallbacks().RegisterToolResultMessages(func(context.Context, *tool.ToolResultMessagesInput) (any, error) {
				augmented = true
				return model.Message{Role: model.RoleTool, ToolID: "1", Content: "augmented"}, nil
			})
			turn := &turn{config: RunConfig{Hooks: hooks, AugmentCallbacks: augment}}
			messages, err := turn.callbacks().RunToolResultMessages(t.Context(), &tool.ToolResultMessagesInput{ToolName: "http_request", ToolCallID: "1"})
			if augmented != tc.wantAugment || !errors.Is(err, tc.err) {
				t.Fatalf("augmentation=%v err=%v", augmented, err)
			}
			if err == nil {
				message, ok := messages.(model.Message)
				if !ok || message.ToolID != "1" || (tc.wantAugment && message.Content != "augmented") || (!tc.wantAugment && message.Content != "hook") {
					t.Fatalf("lost native tool message: %#v", messages)
				}
			}
		})
	}
}
