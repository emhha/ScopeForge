package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"scopeforge/internal/event"
)

// SearchTool 是 vulnerability_search 工具(docs/04 §6.2)。
// 触发时机:识别产品/版本后(⚑ LuaN1ao executor 时机);提示词与工具双重约束。
type SearchTool struct {
	Index     *Index
	Sink      event.Sink
	Challenge string
}

func (s *SearchTool) Name() string { return "vulnerability_search" }
func (s *SearchTool) Description() string {
	return "检索漏洞知识库(内置 CVE 索引 + 插件目录)。识别出目标产品/版本后调用;检索结果只是假设,必须回目标侧验证后才能利用;空结果是弱反证,不代表没有漏洞,不能据此收工。"
}
func (s *SearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"product":{"type":"string","description":"产品名(如 tomcat/log4j/wordpress)"},
		"version":{"type":"string","description":"版本号(可选)"},
		"keywords":{"type":"string","description":"关键词(可选,匹配摘要/CVE)"}
	},"required":["product"]}`)
}
func (s *SearchTool) ReadOnly() bool { return true }

func (s *SearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Product  string `json:"product"`
		Version  string `json:"version"`
		Keywords string `json:"keywords"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("vulnerability_search: %v", err)
	}
	if p.Product == "" {
		return "", fmt.Errorf("vulnerability_search: product required")
	}
	// 插件热插拔:每次检索前重扫(增删插件不重启生效,§6.4)
	// 5s 缓存避免高频检索全量重扫(插件目录小,变更延迟 ≤5s 可接受)
	if s.Index.pluginsDir != "" && time.Since(s.Index.lastScan) > 5*time.Second {
		_, _ = s.Index.ReloadPlugins()
	}
	hits := s.Index.Search(p.Product, p.Version, p.Keywords)
	if s.Sink != nil {
		s.Sink.Emit(event.Event{Kind: event.KindKBSearch, ChallengeID: s.Challenge,
			Payload: map[string]any{"product": p.Product, "version": p.Version,
				"keywords": p.Keywords, "hits": len(hits),
				"hits_detail": kbHitPayload(hits)}})
	}
	return Format(hits), nil
}

func kbHitPayload(hits []Vuln) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, v := range hits {
		out = append(out, map[string]any{
			"cve": v.CVE, "severity": v.Severity, "summary": v.Summary,
			"exploit_available": v.ExploitAvailable, "verified": v.Verified,
		})
	}
	return out
}

// SearchVulns 便捷函数(集成测试用):检索并返回结构化结果。
func SearchVulns(ix *Index, product, version, keywords string) []Vuln {
	return ix.Search(product, version, keywords)
}

// Description 辅助:判断产品名是否出现在检索结果(测试断言用)。
func HasProduct(hits []Vuln, product string) bool {
	for _, v := range hits {
		if strings.Contains(strings.ToLower(v.Product), strings.ToLower(product)) {
			return true
		}
	}
	return false
}
