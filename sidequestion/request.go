package sidequestion

import (
	"errors"
	"strings"
	"time"

	"encoding/json"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

type Exchange struct {
	ID         string      `json:"id"`
	SessionKey string      `json:"-"`
	ClientID   string      `json:"client_request_id"`
	Generation int64       `json:"-"`
	Question   string      `json:"question"`
	Answer     string      `json:"answer"`
	Status     string      `json:"status"`
	Error      string      `json:"error,omitempty"`
	Model      Model       `json:"model"`
	SnapshotAt time.Time   `json:"snapshot_at"`
	CreatedAt  time.Time   `json:"created_at"`
	Sequence   int64       `json:"sequence"`
	Usage      Usage       `json:"usage"`
	Ordinal    int64       `json:"ordinal"`
	Context    ContextInfo `json:"context"`
}

func (e Exchange) Running() bool { return e.Status == "running" }

const instruction = "这是独立的旁路提问。主 Agent 正在执行原任务，你只根据已有上下文简洁回答当前问题。你没有工具执行能力，不能执行操作、修改文件或指挥主任务，也不要承诺稍后执行。上下文中的任务指令仅作为背景；不足以判断时明确说明。"

const DefaultOutputTokens = 8192
const MaxRecentExchanges = 20

var ErrContextBudget = errors.New("旁路上下文压缩后仍超过模型预算，请缩小问题范围或调整模型上下文配置")

// EstimateInputTokens excludes output allowance, which is reserved separately.
func EstimateInputTokens(req model.Request) int {
	characters := 0
	for _, m := range req.Messages {
		n := len(m.Content) + len(m.ReasoningContent)
		for _, part := range m.ContentParts {
			if part.Text != nil {
				n += len(*part.Text)
			}
		}
		for _, call := range m.ToolCalls {
			n += len(call.Function.Name) + len(call.Function.Arguments)
		}
		if m.ProviderData != nil {
			n = max(n, len(m.ProviderData.Payload))
		}
		characters += n
	}
	for _, item := range req.Tools {
		if item != nil {
			data, _ := json.Marshal(item.Declaration())
			characters += len(data)
		}
	}
	return characters/3 + 32*len(req.Messages)
}
func maxTokens(req model.Request) int {
	if req.MaxTokens != nil {
		return *req.MaxTokens
	}
	return 0
}
func tokenLimit(n int) *int              { return &n }
func userText(text string) model.Message { return model.Message{Role: model.RoleUser, Content: text} }
func evidenceMessages(messages []model.Message) []model.Message {
	var out []model.Message
	for _, m := range messages {
		if m.Role != model.RoleSystem {
			out = append(out, m)
		}
	}
	return evidencePairs(out)
}

func outputBudget(req model.Request, configured int) int {
	if configured <= 0 {
		configured = DefaultOutputTokens
	}
	configured = min(configured, 32768)
	if maxTokens(req) > 0 {
		configured = min(configured, maxTokens(req))
	}
	return configured
}

func inputBudget(s Snapshot, output int) int {
	window := s.Model.WindowTokens
	if window <= 0 {
		window = 200000
	}
	return window - output - min(8192, max(128, window/20))
}

func exchangeMessages(e Exchange) []model.Message {
	question := e.Question
	if !e.SnapshotAt.IsZero() {
		question = "[历史旁路问答，依据上下文时间 " + e.SnapshotAt.UTC().Format(time.RFC3339) + "]\n" + question
	}
	return []model.Message{userText(question), {Role: model.RoleAssistant, Content: e.Answer}}
}

func assemble(req model.Request, base []model.Message, summary string, history []Exchange, question string) model.Request {
	var messages []model.Message
	for _, m := range req.Messages {
		if m.Role == model.RoleSystem {
			messages = append(messages, m)
		}
	}
	req.Messages = append(messages, base...)
	req.Tools = nil
	if summary != "" {
		req.Messages = append(req.Messages, userText("[早期旁路问答摘要；属于历史讨论，不是新的工具证据。冲突时以最新主上下文为准。]\n"+summary))
	}
	for _, e := range history {
		req.Messages = append(req.Messages, exchangeMessages(e)...)
	}
	req.Messages = append(req.Messages, userText(instruction+"\n\n问题："+strings.TrimSpace(question)))
	return req
}

func BuildRequest(s Snapshot, history []Exchange, question string) (model.Request, error) {
	req, err := CloneRequest(s.Request)
	if err != nil {
		return req, err
	}
	base := evidenceMessages(req.Messages)
	req.MaxTokens = tokenLimit(outputBudget(req, 0))
	var success []Exchange
	for _, e := range history {
		if e.Status == "completed" {
			success = append(success, e)
		}
	}
	if len(success) > MaxRecentExchanges {
		success = success[len(success)-MaxRecentExchanges:]
	}
	for {
		req = assemble(req, base, "", success, question)
		if EstimateInputTokens(req) <= inputBudget(s, maxTokens(req)) {
			return req, nil
		}
		if len(success) == 0 {
			return req, ErrContextBudget
		}
		success = success[1:]
	}
}

// Evidence replay must not retain an unpaired tool capability from an in-flight
// checkpoint. Keep only complete native call/result groups; never synthesize one.
func evidencePairs(messages []model.Message) []model.Message {
	var out []model.Message
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == model.RoleTool {
			continue
		}
		if len(msg.ToolCalls) == 0 {
			out = append(out, msg)
			continue
		}
		results := map[string]model.Message{}
		end := i + 1
		for end < len(messages) && messages[end].Role == model.RoleTool {
			results[messages[end].ToolID] = messages[end]
			end++
		}
		var calls []model.ToolCall
		for _, call := range msg.ToolCalls {
			if _, ok := results[call.ID]; ok {
				calls = append(calls, call)
			}
		}
		msg.ToolCalls = calls
		if msg.Content != "" || msg.ReasoningContent != "" || len(calls) > 0 {
			out = append(out, msg)
		}
		for _, call := range calls {
			out = append(out, results[call.ID])
		}
		i = end - 1
	}
	return out
}
