package sidequestion

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

func replayFixture(history []Exchange, memory *Memory) Replay {
	return Replay{Memory: *memory, Load: func(_ context.Context, after int64) ([]Exchange, error) {
		var page []Exchange
		for _, e := range history {
			if e.Ordinal > after && e.Status == "completed" {
				page = append(page, e)
			}
			if len(page) == 20 {
				break
			}
		}
		return page, nil
	}, Save: func(_ context.Context, value Memory) error { *memory = value; return nil }}
}

func TestSideTwentyExchangeBoundaryAndRestart(t *testing.T) {
	for _, n := range []int{19, 20, 21, 50} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var history []Exchange
			for i := 1; i <= n; i++ {
				history = append(history, Exchange{Ordinal: int64(i), Question: fmt.Sprintf("question-%d", i), Answer: "historical answer", Status: "completed", SnapshotAt: time.Unix(int64(i), 0)})
			}
			history[0].Answer = "EARLY-DECISION-ALPHA"
			var requests []model.Request
			summaries := 0
			p := fakeModel{complete: func(_ context.Context, r model.Request) (model.Message, string, model.Usage, error) {
				if isSummary(r) {
					summaries++
					if !strings.Contains(r.Messages[1].Content, "EARLY-DECISION-ALPHA") {
						t.Fatal("rolling update lost prior summary")
					}
					return assistant("EARLY-DECISION-ALPHA; historical discussion, not current evidence"), "end_turn", model.Usage{PromptTokens: 11}, nil
				}
				requests = append(requests, r)
				return assistant("answer"), "end_turn", model.Usage{PromptTokens: 7}, nil
			}}
			snap := Snapshot{Request: fixture(), Model: Model{WindowTokens: 200000}}
			var memory Memory
			out, info, err := (SideQuestionService{Model: p}).Respond(t.Context(), snap, "what was my first decision?", replayFixture(history, &memory), ContextOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if info.RecentExchanges != min(n, 20) || memory.Through != int64(max(0, n-20)) {
				t.Fatalf("boundary: %+v %+v", info, memory)
			}
			if out.Usage.PromptTokens != 11*summaries+7 {
				t.Fatal("summary usage missing")
			}
			if !strings.Contains(string(mustJSON(t, requests[0])), "EARLY-DECISION-ALPHA") {
				t.Fatal("first-round decision absent after round 20")
			}
			before := summaries
			_, _, err = (SideQuestionService{Model: p}).Respond(t.Context(), snap, "continue after restart", replayFixture(history, &memory), ContextOptions{}, nil)
			if err != nil || summaries != before {
				t.Fatalf("persisted summary not reused: %v", err)
			}
		})
	}
}

func TestSideBudgetCountsContentNotJSONCharacters(t *testing.T) {
	snap := Snapshot{Request: fixture(), Model: Model{WindowTokens: 200000}}
	snap.Request.MaxTokens = tokenLimit(32768)
	snap.Request.Messages = append(snap.Request.Messages, userText(strings.Repeat("<script>const result = api.response;</script>\n", 5500)))
	before := mustJSON(t, snap)
	if len(before) < 200000 {
		t.Fatal("fixture too small to exercise regression")
	}
	req, err := BuildRequest(snap, nil, "progress?")
	if err != nil {
		t.Fatalf("valid code-heavy snapshot rejected: %v", err)
	}
	if maxTokens(req) != 8192 || EstimateInputTokens(req) >= 200000 {
		t.Fatalf("budget: %d %d", maxTokens(req), EstimateInputTokens(req))
	}
	if string(before) != string(mustJSON(t, snap)) {
		t.Fatal("snapshot mutated")
	}
	baseline := EstimateInputTokens(model.Request{Messages: []model.Message{userText("x")}})
	if EstimateInputTokens(model.Request{Messages: []model.Message{{Role: model.RoleSystem, Content: strings.Repeat("中", 3000)}, userText("x")}, Tools: fixture().Tools}) <= baseline+3000 {
		t.Fatal("system/schema/CJK omitted")
	}
}

func TestSideLongExchangeAndSnapshotCompactionAreBounded(t *testing.T) {
	for _, hugeHistory := range []bool{true, false} {
		t.Run(fmt.Sprint(hugeHistory), func(t *testing.T) {
			snap := Snapshot{Request: fixture(), Model: Model{WindowTokens: 32000}}
			var history []Exchange
			if hugeHistory {
				history = []Exchange{{Ordinal: 1, Question: "earlier", Answer: strings.Repeat("历史依据ABC", 14000), Status: "completed"}}
			} else {
				snap.Request.Messages = append([]model.Message{userText(strings.Repeat("old tool evidence 中文", 9000))}, snap.Request.Messages...)
			}
			original := mustJSON(t, snap)
			calls, answers := 0, 0
			var final model.Request
			p := fakeModel{complete: func(_ context.Context, r model.Request) (model.Message, string, model.Usage, error) {
				if isSummary(r) {
					calls++
					if len(r.Tools) != 0 || EstimateInputTokens(r)+maxTokens(r) > 32000 || !utf8.ValidString(r.Messages[len(r.Messages)-1].Content) {
						t.Fatal("unsafe summary request")
					}
					return assistant("goal, evidence and unresolved questions"), "end_turn", model.Usage{CompletionTokens: 3}, nil
				}
				answers++
				final = r
				return assistant("done"), "end_turn", model.Usage{CompletionTokens: 5}, nil
			}}
			var memory Memory
			out, info, err := (SideQuestionService{Model: p}).Respond(t.Context(), snap, "progress?", replayFixture(history, &memory), ContextOptions{}, nil)
			if err != nil || calls < 2 || calls > 12 || answers != 1 || out.Usage.CompletionTokens != 3*calls+5 {
				t.Fatalf("calls=%d answers=%d info=%+v err=%v", calls, answers, info, err)
			}
			if info.EstimatedInputTokens > info.InputBudget {
				t.Fatal("oversized final request")
			}
			if !reflect.DeepEqual(final.Messages, evidencePairs(final.Messages)) {
				t.Fatal("tool pairing broken")
			}
			if !strings.Contains(string(mustJSON(t, final)), "verified asset") {
				t.Fatal("recent tool evidence discarded")
			}
			if string(original) != string(mustJSON(t, snap)) {
				t.Fatal("main snapshot modified")
			}
			before := calls
			_, _, err = (SideQuestionService{Model: p}).Respond(t.Context(), snap, "another question", replayFixture(history, &memory), ContextOptions{}, nil)
			if err != nil || calls != before {
				t.Fatalf("summary cache not reused: %v", err)
			}
			if !hugeHistory {
				snap.Version++
				_, _, err = (SideQuestionService{Model: p}).Respond(t.Context(), snap, "new snapshot", replayFixture(history, &memory), ContextOptions{}, nil)
				if err != nil || calls == before {
					t.Fatalf("stale snapshot cache reused: %v", err)
				}
			}
		})
	}
}

func TestSideOverflowRecoveryOnceAndFailureUsage(t *testing.T) {
	for _, twice := range []bool{false, true} {
		t.Run(fmt.Sprint(twice), func(t *testing.T) {
			answers, summaries := 0, 0
			p := fakeModel{complete: func(_ context.Context, r model.Request) (model.Message, string, model.Usage, error) {
				if isSummary(r) {
					summaries++
					return assistant("summary"), "end_turn", model.Usage{PromptTokens: 3}, nil
				}
				answers++
				if answers == 1 || twice {
					return model.Message{}, "", model.Usage{PromptTokens: 5}, &model.ResponseError{Code: nativeCode("context_length_exceeded"), Message: "context limit"}
				}
				return assistant("recovered"), "end_turn", model.Usage{PromptTokens: 7}, nil
			}}
			snap := Snapshot{Request: fixture(), Model: Model{WindowTokens: 32000}}
			snap.Request.Messages = append([]model.Message{userText(strings.Repeat("old context ", 2000))}, snap.Request.Messages...)
			out, info, err := (SideQuestionService{Model: p}).Respond(t.Context(), snap, "question", Replay{}, ContextOptions{}, nil)
			if answers != 2 || summaries != 1 || !info.OverflowRetried || (err != nil) != twice {
				t.Fatalf("retry limit: %d %d %+v %v", answers, summaries, info, err)
			}
			want := 15
			if twice {
				want = 13
			}
			if out.Usage.PromptTokens != want {
				t.Fatalf("usage=%+v", out.Usage)
			}
		})
	}
	// A stream that already exposed text must never replay it as a fresh answer.
	count := 0
	p := fakeModel{stream: func(context.Context, model.Request) iter.Seq2[*model.Response, error] {
		return func(y func(*model.Response, error) bool) {
			count++
			y(partial("partial"), nil)
			y(&model.Response{}, &model.ResponseError{Code: nativeCode("context_length_exceeded"), Message: "context limit"})
		}
	}}
	out, _, err := (SideQuestionService{Model: p}).Respond(t.Context(), Snapshot{Request: fixture(), Model: Model{Streaming: true}}, "q", Replay{}, ContextOptions{}, nil)
	if err == nil || out.Text != "partial" || count != 1 {
		t.Fatal("partial stream retried")
	}
}

func TestSideSummaryCancellationAndFailureDoNotAdvanceMemory(t *testing.T) {
	for _, mode := range []string{"cancel", "error", "empty", "truncated", "too-large", "tool", "save-failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			p := fakeModel{complete: func(_ context.Context, r model.Request) (model.Message, string, model.Usage, error) {
				calls++
				usage := model.Usage{PromptTokens: 9}
				if !isSummary(r) {
					t.Fatal("failed summary continued to answer")
				}
				switch mode {
				case "cancel":
					cancel()
					return assistant("late"), "end_turn", usage, nil
				case "error":
					return model.Message{}, "", usage, errors.New("unavailable")
				case "empty":
					return assistant(""), "end_turn", usage, nil
				case "truncated":
					return assistant("partial"), "length", usage, nil
				case "too-large":
					return assistant(strings.Repeat("中", 5000)), "end_turn", usage, nil
				case "tool":
					return model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "forbidden", Function: model.FunctionDefinitionParam{Name: "Bash"}}}}, "tool_use", usage, nil
				}
				return assistant("summary"), "end_turn", usage, nil
			}}
			var history []Exchange
			for i := 1; i <= 21; i++ {
				history = append(history, Exchange{Ordinal: int64(i), Question: "q", Answer: "a", Status: "completed"})
			}
			var memory Memory
			replay := replayFixture(history, &memory)
			if mode == "save-failed" {
				replay.Save = func(context.Context, Memory) error { return errors.New("cleared") }
			}
			out, _, err := (SideQuestionService{Model: p}).Respond(ctx, Snapshot{Request: fixture()}, "q", replay, ContextOptions{}, nil)
			if err == nil || calls != 1 || memory.Through != 0 || out.Usage.PromptTokens != 9 {
				t.Fatalf("failed preparation: %+v %+v %v", memory, out, err)
			}
		})
	}
}

func TestSideSummaryCallCapAndStaticBudget(t *testing.T) {
	calls := 0
	p := fakeModel{complete: func(_ context.Context, r model.Request) (model.Message, string, model.Usage, error) {
		calls++
		if !isSummary(r) {
			t.Fatal("oversized request answered")
		}
		return assistant("bounded summary"), "end_turn", model.Usage{CompletionTokens: 1}, nil
	}}
	snap := Snapshot{Model: Model{WindowTokens: 32000}, Request: fixture()}
	snap.Request.Messages = []model.Message{userText(strings.Repeat("x", 1200000))}
	out, _, err := (SideQuestionService{Model: p}).Respond(t.Context(), snap, "q", Replay{}, ContextOptions{}, nil)
	if err == nil || calls != 12 || out.Usage.CompletionTokens != 12 {
		t.Fatalf("summary cap calls=%d err=%v", calls, err)
	}
	calls = 0
	snap.Request.Messages = append([]model.Message{{Role: model.RoleSystem, Content: strings.Repeat("x", 200000)}}, snap.Request.Messages...)
	_, _, err = (SideQuestionService{Model: p}).Respond(t.Context(), snap, "q", Replay{}, ContextOptions{}, nil)
	if !errors.Is(err, ErrContextBudget) || calls != 0 {
		t.Fatalf("static oversized budget: calls=%d err=%v", calls, err)
	}
}

func TestSideTwentyLongRepliesRespectSmallWindow(t *testing.T) {
	var history []Exchange
	for i := 1; i <= 20; i++ {
		history = append(history, Exchange{Ordinal: int64(i), Question: fmt.Sprintf("question-%d", i), Answer: strings.Repeat("evidence ", 3000), Status: "completed"})
	}
	calls := 0
	p := fakeModel{complete: func(_ context.Context, req model.Request) (model.Message, string, model.Usage, error) {
		if EstimateInputTokens(req)+maxTokens(req) > 32000 {
			t.Fatal("request exceeds small model window")
		}
		if isSummary(req) {
			calls++
			return assistant("historical objectives and evidence"), "end_turn", model.Usage{}, nil
		}
		return assistant("answer"), "end_turn", model.Usage{}, nil
	}}
	var memory Memory
	_, info, err := (SideQuestionService{Model: p}).Respond(t.Context(), Snapshot{Request: fixture(), Model: Model{WindowTokens: 32000}}, "current status?", replayFixture(history, &memory), ContextOptions{}, nil)
	if err != nil || !info.HistorySummarized || calls > 12 || memory.Through != 20 {
		t.Fatalf("20 long replies: calls=%d info=%+v memory=%+v err=%v", calls, info, memory, err)
	}
}

func TestSideOverflowWithoutReductionDoesNotRepeatAnswer(t *testing.T) {
	answers, summaries := 0, 0
	p := fakeModel{complete: func(_ context.Context, req model.Request) (model.Message, string, model.Usage, error) {
		if isSummary(req) {
			summaries++
			return assistant(strings.Repeat("longer summary ", 100)), "end_turn", model.Usage{PromptTokens: 3}, nil
		}
		answers++
		return model.Message{}, "", model.Usage{PromptTokens: 5}, &model.ResponseError{Code: nativeCode("context_length_exceeded"), Message: "context limit"}
	}}
	snapshot := Snapshot{Request: model.Request{GenerationConfig: model.GenerationConfig{MaxTokens: tokenLimit(128)}, Messages: []model.Message{userText("tiny")}}}
	out, _, err := (SideQuestionService{Model: p}).Respond(t.Context(), snapshot, "question", Replay{}, ContextOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "未能进一步缩减") || answers != 1 || summaries != 1 || out.Usage.PromptTokens != 8 {
		t.Fatalf("no-progress recovery: %d %d %+v %v", answers, summaries, out, err)
	}
}
