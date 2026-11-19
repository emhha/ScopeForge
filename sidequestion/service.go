package sidequestion

import (
	"context"
	"errors"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// SideQuestionService is a single tools-free native model call, not an Agent.
// Respond adds only the existing bounded context preparation/recovery policy.
type SideQuestionService struct {
	Model  model.Model
	OnCall func(Answer, time.Duration, error)
}

type Answer struct {
	Text         string
	Usage        model.Usage
	ToolUse      bool
	FinishReason string
}

func (s SideQuestionService) Answer(ctx context.Context, req model.Request, streaming bool, update func(Answer)) (out Answer, err error) {
	started := time.Now()
	defer func() {
		if s.OnCall != nil {
			s.OnCall(out, time.Since(started), err)
		}
	}()
	req.Tools = nil
	req.Stream = streaming
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	source, err := s.Model.GenerateContent(attemptCtx, &req)
	if source == nil {
		if err == nil {
			err = errors.New("模型没有返回响应通道")
		}
		return out, err
	}
	// Join the native producer even on startup failure/cancellation. Late usage
	// still belongs to this call and must be metered before releasing admission.
	observeUsage := func(ev *model.Response) {
		if ev != nil && ev.Usage != nil {
			mergeUsage(&out.Usage, *ev.Usage)
		}
	}
	defer func() {
		cancel()
		for ev := range source {
			observeUsage(ev)
		}
	}()
	if err != nil {
		return out, err
	}
	complete := false
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case ev, ok := <-source:
			if !ok {
				break loop
			}
			if ev == nil {
				continue
			}
			observeUsage(ev)
			if ev.Error != nil {
				err = ev.Error
			}
			for _, choice := range ev.Choices {
				if choice.FinishReason != nil {
					out.FinishReason = *choice.FinishReason
				}
				if ev.IsPartial {
					out.Text += choice.Delta.Content
					out.ToolUse = out.ToolUse || len(choice.Delta.ToolCalls) > 0
				} else if choice.Message.Role == model.RoleAssistant {
					if choice.Message.Content != "" {
						out.Text = choice.Message.Content
					}
					out.ToolUse = out.ToolUse || len(choice.Message.ToolCalls) > 0
				}
			}
			complete = complete || ev.Done
			if update != nil {
				update(out)
			}
			if err != nil {
				break loop
			}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && !complete {
		err = errors.New("模型响应中断，请重新提问")
	}
	if err == nil && strings.TrimSpace(out.Text) == "" {
		if out.ToolUse {
			out.Text = "当前旁路提问不能执行工具操作，请在主会话中发出操作请求。"
		} else {
			err = errors.New("模型没有返回回答")
		}
	}
	return out, err
}
