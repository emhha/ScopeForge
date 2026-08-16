package scheduler

import (
	"scopeforge/internal/blackboard"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/kb"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/skill"
	"scopeforge/internal/reasonix/tool"
)

// workerRegistryFor 构造 worker 执行用的注册表(基础 + 平台工具)。
// 平台工具的提交纪律经 Dispatcher(ReporterAdapter),架构红线 A6。
func (s *Scheduler) workerRegistryFor(rec *blackboard.Worker) *tool.Registry {
	reg := tool.NewRegistry()
	if s.registry != nil {
		for _, name := range s.registry.Names() {
			if recursiveTools[name] {
				continue
			}
			if tl, ok := s.registry.Get(name); ok {
				reg.Add(tl)
			}
		}
	}
	// 漏洞账本工具:submit_vulnerability 写本地账本(阶段 2.9/2.11:
	// 不做平台对接,提交语义由 skill 卡承载;回执经 UpdateVulnerabilityReceipt 推进)。
	for _, tl := range dispatcher.NewVulnerabilityTools(rec.ChallengeID, s.sink, rec.ID, s.disp) {
		reg.Add(tl)
	}
	// 知识库检索(vulnerability_search,§6.2)
	if s.kb != nil {
		reg.Add(&kb.SearchTool{Index: s.kb, Sink: s.sink, Challenge: rec.ChallengeID})
	}
	// 层 3 skill 卡热插拔(04 §1/§3):run_skill 按需加载卡体——inline 卡
	// 直接返回卡内容;subagent 卡无 runner 干净报错。此前 worker 注册表
	// 拿不到 run_skill,提示词指路成死路(docs/phase2/11 §2.4)。
	// 注意:recursiveTools 的排除不变——主 registry 的递归工具仍不进 worker,
	// 此处是显式构造的 worker 专用实例(runner=nil,无派生子代理能力)。
	if s.skills != nil {
		reg.Add(skill.NewRunSkillTool(s.skills, nil))
	}
	return reg
}

// skillIndexBlock 渲染技能索引(一次性构建并缓存;经 executor.Options.SkillIndex
// 进入稳定系统前缀,worker 据此发现并用 run_skill 加载卡体)。
func (s *Scheduler) skillIndexBlock() string {
	if s.skills == nil {
		return ""
	}
	s.skillIndexOnce.Do(func() {
		s.skillIndexStr = skill.IndexBlock(s.skills.List())
	})
	return s.skillIndexStr
}

// 递归工具(worker 不派生子代理,防无限递归)。
var recursiveTools = map[string]bool{
	"task": true, "read_only_task": true, "fleet": true,
	"run_skill": true, "read_only_skill": true,
}

func intentIDOf(h dispatcher.Handoff) int64 { return h.IntentID }

func priorityTag(p string) string {
	if p == "" {
		return ""
	}
	return "(" + p + ")"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ------------------------------------------------------------------ 辅助

// providerFor 按 role_map 路由 provider。
// Operator 按 phase 路由(scout/executor/analyst 可配不同模型),Synthesizer 独立。
func (s *Scheduler) providerFor(workerType string, phase Phase) provider.Provider {
	key := workerType
	if workerType == WorkerOperator {
		key = string(phase)
	}
	if name, ok := s.roleMap[key]; ok {
		if p, ok2 := s.providers[name]; ok2 {
			return p
		}
	}
	// 默认:第一个
	for _, p := range s.providers {
		return p
	}
	return nil
}

// workerTurns 返回 worker 最大轮数。
// Operator 按 phase 查 WorkerTurns(键: scout/executor/analyst),Synthesizer 独立键。
func (s *Scheduler) workerTurns(workerType string, phase Phase) int {
	key := workerType
	if workerType == WorkerOperator {
		key = string(phase)
	}
	if n, ok := s.cfg.WorkerTurns[key]; ok && n > 0 {
		return n
	}
	if workerType == WorkerSynthesizer {
		return 10
	}
	switch phase {
	case PhaseRecon, PhaseReason:
		return 15
	default:
		return 30
	}
}
