package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
	"scopeforge/internal/reasonix/tool"
)

// VulnerabilityReporter 是漏洞账本记录回调(工具 → Dispatcher,防包循环)。
// 阶段 2.9/2.11 定案:不做平台对接适配器,提交语义由 skill 卡承载;
// 本回调只负责把漏洞写入本地账本(submitted),回执经 UpdateVulnerabilityReceipt 推进。
type VulnerabilityReporter interface {
	ReportVulnerability(ctx context.Context, workerID, challengeID string, v blackboard.VulnerabilityIn) (*blackboard.Vulnerability, error)
}

// NewVulnerabilityTools 构建漏洞账本工具集(phase2:submit_vulnerability)。
// 不做平台对接:工具只写本地账本,提交语义由 skill 卡指导 agent 用通用工具完成。
func NewVulnerabilityTools(challengeID string, sink event.Sink, workerID string, reporter VulnerabilityReporter) []tool.Tool {
	_ = sink
	t := &vulnTool{defaultChallengeID: challengeID, workerID: workerID, reporter: reporter}
	return []tool.Tool{
		&toolImpl{name: "submit_vulnerability", desc: "记录漏洞到本地账本(输出报告的数据源)。真实提交由你按目标 skill 卡的提交语义用通用工具完成;本工具只负责把漏洞登记进账本(submitted),回执状态由人工/外部确认后推进。", schema: `{"type":"object","properties":{"challenge_id":{"type":"string"},"cwe":{"type":"string"},"asset":{"type":"string"},"endpoint":{"type":"string"},"severity":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"}},"required":["asset","title"]}`, ro: false,
			run: t.submitVulnerability},
	}
}

// vulnTool 是漏洞账本工具执行体。
type vulnTool struct {
	defaultChallengeID string
	workerID           string
	reporter           VulnerabilityReporter
}

func (t *vulnTool) submitVulnerability(ctx context.Context, args map[string]any) (string, error) {
	id, err := t.challengeID(args)
	if err != nil {
		return "", err
	}
	v := blackboard.VulnerabilityIn{
		CWE:         str(args["cwe"]),
		Asset:       str(args["asset"]),
		Endpoint:    str(args["endpoint"]),
		Severity:    str(args["severity"]),
		Title:       str(args["title"]),
		Description: str(args["description"]),
	}
	if v.Asset == "" || v.Title == "" {
		return "", fmt.Errorf("submit_vulnerability: asset 与 title 必填")
	}
	if t.reporter != nil {
		entry, err := t.reporter.ReportVulnerability(ctx, t.workerID, id, v)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("账本记录: %s (id=%d)%s", entry.Status, entry.ID, receiptHint(entry.Status)), nil
	}
	return "submit_vulnerability: 未配置账本通道(仅记录 submitted)", nil
}

// challengeID 解析:args 覆盖,否则用工具默认。
func (t *vulnTool) challengeID(args map[string]any) (string, error) {
	if id, ok := args["challenge_id"].(string); ok && id != "" {
		return id, nil
	}
	if t.defaultChallengeID != "" {
		return t.defaultChallengeID, nil
	}
	return "", fmt.Errorf("challenge_id required (no default)")
}

// receiptHint 账本状态的操作提示(工具输出给 agent 的行为引导)。
func receiptHint(status string) string {
	switch status {
	case blackboard.LedgerAccepted:
		return " [已确认,该漏洞完成]"
	case blackboard.LedgerDuplicate:
		return " [重复:该漏洞已提交过,不要再重复提交]"
	case blackboard.LedgerFalsePositive:
		return " [误报:方向可调整验证姿势后重试]"
	case blackboard.LedgerRejected:
		return " [拒绝:限频或参数问题,退避后重试]"
	default:
		return ""
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toolImpl 是通用工具载体(仿原 platform 工具基类)。
type toolImpl struct {
	name   string
	desc   string
	schema string
	ro     bool
	run    func(ctx context.Context, args map[string]any) (string, error)
}

func (t *toolImpl) Name() string        { return t.name }
func (t *toolImpl) Description() string { return t.desc }
func (t *toolImpl) Schema() json.RawMessage {
	return json.RawMessage(t.schema)
}
func (t *toolImpl) ReadOnly() bool { return t.ro }

func (t *toolImpl) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", fmt.Errorf("%s: bad args: %v", t.name, err)
		}
	}
	return t.run(ctx, m)
}
