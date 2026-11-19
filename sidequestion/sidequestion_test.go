package sidequestion

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type fakeModel struct {
	stream   func(context.Context, model.Request) iter.Seq2[*model.Response, error]
	complete func(context.Context, model.Request) (model.Message, string, model.Usage, error)
}

func (fakeModel) Info() model.Info { return model.Info{Name: "test"} }
func (p fakeModel) GenerateContent(ctx context.Context, r *model.Request) (<-chan *model.Response, error) {
	out := make(chan *model.Response)
	go func() {
		defer close(out)
		send := func(response *model.Response, err error) bool {
			if err != nil {
				response = &model.Response{Error: &model.ResponseError{Message: err.Error()}}
				if native, ok := err.(*model.ResponseError); ok {
					response.Error = native
				}
			}
			select {
			case out <- response:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if r.Stream && p.stream != nil {
			p.stream(ctx, *r)(send)
			return
		}
		if p.complete == nil {
			p.stream(ctx, *r)(send)
			return
		}
		msg, stop, usage, err := p.complete(ctx, *r)
		ev := &model.Response{Done: true, Choices: []model.Choice{{Message: msg, FinishReason: &stop}}, Usage: &usage}
		if err != nil {
			ev.Error = &model.ResponseError{Message: err.Error()}
			if native, ok := err.(*model.ResponseError); ok {
				ev.Error = native
			}
		}
		// A native producer can expose final spend after cancellation; the
		// consumer must drain it, as the former atomic contract required.
		out <- ev
	}()
	return out, nil
}
func partial(text string) *model.Response {
	return &model.Response{IsPartial: true, Choices: []model.Choice{{Delta: model.Message{Content: text}}}}
}
func completed(text string) *model.Response {
	return &model.Response{Done: true, Choices: []model.Choice{{Message: assistant(text)}}}
}
func streamRequest() model.Request { r := fixture(); r.Stream = true; return r }
func drainModel(ctx context.Context, m model.Model, req model.Request) error {
	source, err := m.GenerateContent(ctx, &req)
	if err != nil {
		return err
	}
	for ev := range source {
		if ev != nil && ev.Error != nil {
			err = ev.Error
		}
	}
	return err
}

type testTool struct{ declaration tool.Declaration }

func (t *testTool) Declaration() *tool.Declaration { return &t.declaration }
func assistant(text string) model.Message {
	return model.Message{Role: model.RoleAssistant, Content: text}
}
func fixture() model.Request {
	temp := 0.3
	return model.Request{GenerationConfig: model.GenerationConfig{Temperature: &temp, MaxTokens: tokenLimit(128)}, Messages: []model.Message{
		{Role: model.RoleSystem, Content: "system"},
		userText("<system-reminder>date</system-reminder>"), userText("read asset"),
		{Role: model.RoleAssistant, ReasoningContent: "reason", ReasoningSignature: "signed", ToolCalls: []model.ToolCall{{ID: "tool-1", Type: "function", Function: model.FunctionDefinitionParam{Name: "read", Arguments: []byte(`{"url":"fixture"}`)}}}},
		{Role: model.RoleTool, ToolID: "tool-1", Content: "verified asset"},
	}, Tools: map[string]tool.Tool{"read": &testTool{declaration: tool.Declaration{Name: "read", Description: "read fixture evidence", InputSchema: &tool.Schema{Type: "object", Properties: map[string]*tool.Schema{"url": {Type: "string"}}}}}}}
}

func TestCheckpointDeepCopyAndBoundaries(t *testing.T) {
	var snapshots []Snapshot
	p := Bind(fakeModel{complete: func(context.Context, model.Request) (model.Message, string, model.Usage, error) {
		return assistant("finished"), "end_turn", model.Usage{}, nil
	}}, Model{Model: "actual"})
	ctx := Attach(WithPublisher(t.Context(), func(s Snapshot) { snapshots = append(snapshots, s) }), Parent{ConversationID: 1})
	req := fixture()
	if err := drainModel(ctx, p, req); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[1].Request.Messages[len(snapshots[1].Request.Messages)-1].Content != "finished" {
		t.Fatalf("checkpoints: %+v", snapshots)
	}
	before, _ := json.Marshal(snapshots)
	req.Messages[0].Content = "mutated"
	*req.Temperature = 2

	req.Messages[3].ToolCalls[0].Function.Arguments[0] = 'X'
	req.Messages[4].Content = "mutated"
	req.Tools["read"].Declaration().InputSchema.Properties["url"].Type = "mutated"
	after, _ := json.Marshal(snapshots)
	if string(before) != string(after) {
		t.Fatal("published checkpoint aliases structured input")
	}
	// Auxiliary/compaction calls lack the model-attempt marker.
	_ = drainModel(ctx, p, summaryRequest("", "summary request"))
	if len(snapshots) != 2 {
		t.Fatal("auxiliary completion replaced snapshot")
	}
	// Native completion itself publishes the terminal projection; no second
	// legacy Finish/append hook should duplicate a reminder or tool exchange.
	last := snapshots[len(snapshots)-1]
	if last.Request.Messages[1].Content != "<system-reminder>date</system-reminder>" || len(last.Request.Messages) != 6 {
		t.Fatalf("terminal/reminder duplication: %+v", last.Request.Messages)
	}
	if !strings.Contains(string(mustJSON(t, last)), "verified asset") {
		t.Fatal("terminal lost paired tool result")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestSnapshotExcludesPartialStreamAndSelectsConcreteModel(t *testing.T) {
	var snapshots []Snapshot
	var mu sync.Mutex
	textSeen := make(chan struct{})
	var seenOnce sync.Once
	failed := Bind(fakeModel{stream: func(context.Context, model.Request) iter.Seq2[*model.Response, error] {
		return func(y func(*model.Response, error) bool) {
			y(&model.Response{}, &model.ResponseError{Type: "503", Message: "unavailable"})
		}
	}}, Model{Model: "failed"})
	good := Bind(fakeModel{stream: func(context.Context, model.Request) iter.Seq2[*model.Response, error] {
		return func(y func(*model.Response, error) bool) {
			if !y(partial("complete"), nil) {
				return
			}
			<-textSeen
			y(completed("complete"), nil)
		}
	}}, Model{Model: "selected"})
	// Selection remains outside the checkpoint wrapper. Observe two concrete
	// attempts, just as a product router does, without routing through Provider.
	_ = failed
	ctx := Attach(WithPublisher(t.Context(), func(s Snapshot) { mu.Lock(); defer mu.Unlock(); snapshots = append(snapshots, s) }), Parent{ConversationID: 2})
	if err := drainModel(ctx, failed, streamRequest()); err == nil {
		t.Fatal("failed member did not fail")
	}
	request := streamRequest()
	source, err := good.GenerateContent(ctx, &request)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range source {
		if ev.Error != nil {
			t.Fatal(ev.Error)
		}
		if ev.IsPartial && len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
			mu.Lock()
			partialPublished := strings.Contains(string(mustJSON(t, snapshots[len(snapshots)-1])), "complete")
			mu.Unlock()
			seenOnce.Do(func() { close(textSeen) })
			if partialPublished {
				t.Fatal("partial reply published")
			}
		}
	}
	if last := snapshots[len(snapshots)-1]; last.Model.Model != "selected" || last.Request.Messages[len(last.Request.Messages)-1].Content != "complete" {
		t.Fatalf("wrong selected model/checkpoint: %+v", last)
	}
	// Cancel a second attempt before accepting any output. It may publish
	// request snapshots, but must never publish an assistant reply.
	count := len(snapshots)
	cancelCtx, cancel := context.WithCancel(ctx)
	source, err = good.GenerateContent(cancelCtx, &request)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range source {
	}
	for _, snapshot := range snapshots[count:] {
		if len(snapshot.Request.Messages) != len(fixture().Messages) {
			t.Fatal("cancelled stream published reply")
		}
	}

}

func TestBuildRequestCompactionToolPairingAndBudget(t *testing.T) {
	s := Snapshot{Request: fixture(), Model: Model{WindowTokens: 100000}}
	s.Request.Messages = []model.Message{{Role: model.RoleSystem, Content: "system"}, userText("compressed summary"), {Role: model.RoleTool, ToolID: "orphan", Content: "orphan"}}
	history := []Exchange{}
	for i := 0; i < 25; i++ {
		history = append(history, Exchange{Question: strings.Repeat("q", 100), Answer: "answer", Status: "completed"})
	}
	r, err := BuildRequest(s, history, "current question")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 43 || r.Messages[1].Content != "compressed summary" {
		t.Fatalf("history cap/compaction: %d %+v", len(r.Messages), r.Messages[0])
	}
	for _, m := range r.Messages {
		if m.Role == model.RoleTool {
			t.Fatal("orphan tool result survived")
		}
	}
	s.Model.WindowTokens = 1000
	r, err = BuildRequest(s, history, "current question")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) >= 43 || len(r.Messages) < 2 {
		t.Fatal("budget did not reduce side history")
	}
	s.Model.WindowTokens = 10
	if _, err = BuildRequest(s, nil, "question"); err == nil {
		t.Fatal("oversized base accepted")
	}
}

func TestServiceNoToolsAndUsageOnFailure(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "atomic"}[streaming], func(t *testing.T) {
			calls := 0
			p := fakeModel{complete: func(context.Context, model.Request) (model.Message, string, model.Usage, error) {
				calls++
				return model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "new", Type: "function", Function: model.FunctionDefinitionParam{Name: "run_shell", Arguments: []byte(`{"command":"touch forbidden"}`)}}}}, "tool_use", model.Usage{PromptTokens: 11}, nil
			}, stream: func(context.Context, model.Request) iter.Seq2[*model.Response, error] {
				return func(y func(*model.Response, error) bool) {
					calls++
					y(&model.Response{Usage: &model.Usage{PromptTokens: 11}}, nil)
					y(&model.Response{IsPartial: true, Choices: []model.Choice{{Delta: model.Message{ToolCalls: []model.ToolCall{{ID: "new", Function: model.FunctionDefinitionParam{Name: "run_shell"}}}}}}}, nil)
					y(completed(""), nil)
				}
			}}
			before := fixture()
			req, _ := CloneRequest(before)
			out, err := (SideQuestionService{Model: p}).Answer(t.Context(), req, streaming, nil)
			if err != nil || calls != 1 || !out.ToolUse || !strings.Contains(out.Text, "不能执行工具") || out.Usage.PromptTokens != 11 {
				t.Fatalf("answer %+v calls=%d err=%v", out, calls, err)
			}
			want, _ := CloneRequest(before)
			if !reflect.DeepEqual(want, req) {
				t.Fatal("service mutated main context")
			}
		})
	}
	p := fakeModel{stream: func(context.Context, model.Request) iter.Seq2[*model.Response, error] {
		return func(y func(*model.Response, error) bool) {
			y(&model.Response{Usage: &model.Usage{PromptTokens: 12}}, nil)
			y(partial("partial"), nil)
			y(&model.Response{}, errors.New("broken"))
		}
	}}
	out, err := (SideQuestionService{Model: p}).Answer(t.Context(), fixture(), true, nil)
	if err == nil || out.Text != "partial" || out.Usage.PromptTokens != 12 {
		t.Fatalf("lost failure usage/partial: %+v %v", out, err)
	}
}

func TestMainSideConcurrencyAndIndependentCancellation(t *testing.T) {
	for _, cancelMain := range []bool{false, true} {
		t.Run(map[bool]string{true: "stop-main", false: "stop-side"}[cancelMain], func(t *testing.T) {
			started := make(chan struct{}, 2)
			p := fakeModel{stream: func(ctx context.Context, _ model.Request) iter.Seq2[*model.Response, error] {
				return func(y func(*model.Response, error) bool) {
					y(&model.Response{Usage: &model.Usage{PromptTokens: 7}}, nil)
					started <- struct{}{}
					<-ctx.Done()
					y(&model.Response{}, ctx.Err())
				}
			}}
			mainCtx, stopMain := context.WithCancel(t.Context())
			defer stopMain()
			sideCtx, stopSide := context.WithCancel(t.Context())
			defer stopSide()
			var mu sync.Mutex
			bound := Bind(p, Model{Model: "blocking"})
			mainCtx = Attach(WithPublisher(mainCtx, func(Snapshot) { mu.Lock(); mu.Unlock() }), Parent{ConversationID: 3})
			mainDone, sideDone := make(chan struct{}), make(chan Answer, 1)
			go func() {
				defer close(mainDone)
				request := streamRequest()
				stream, _ := bound.GenerateContent(mainCtx, &request)
				for range stream {
				}
			}()
			go func() {
				a, _ := (SideQuestionService{Model: bound}).Answer(sideCtx, fixture(), true, nil)
				sideDone <- a
			}()
			for i := 0; i < 2; i++ {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("main and side did not execute concurrently")
				}
			}
			if cancelMain {
				stopMain()
				select {
				case <-mainDone:
				case <-time.After(time.Second):
					t.Fatal("main cancellation stuck")
				}
				select {
				case <-sideDone:
					t.Fatal("main cancellation stopped side")
				default:
				}
				stopSide()
			} else {
				stopSide()
				select {
				case a := <-sideDone:
					if a.Usage.PromptTokens != 7 {
						t.Fatal("cancelled usage lost")
					}
				case <-time.After(time.Second):
					t.Fatal("side cancellation stuck")
				}
				select {
				case <-mainDone:
					t.Fatal("side cancellation stopped main")
				default:
				}
				stopMain()
			}
		})
	}
}
