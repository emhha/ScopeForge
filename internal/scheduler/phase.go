package scheduler

// Phase 是 Operator 的运行阶段(docs/phase2/07 §4 合并方案)。
// Operator 三阶段合一(侦察/挖掘/分析),由调度器显式派发到某个 Phase;
// Synthesizer 独立收尾,无 Phase。
// 2.36:phase 字符串值与提示词角色名统一(Scout/Executor/Analyst),
// 旧值 recon/explore/reason 仅存在于历史数据,展示层做兼容映射。
type Phase string

const (
	PhaseRecon   Phase = "scout"    // 首次侦察(角色 Scout)
	PhaseExplore Phase = "executor" // 方向挖掘(角色 Executor)
	PhaseReason  Phase = "analyst"  // 假说分析(角色 Analyst)
)
