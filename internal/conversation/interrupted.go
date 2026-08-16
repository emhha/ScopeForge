package conversation

import (
	"strings"

	"scopeforge/internal/reasonix/provider"
)

// MarkInterrupted 把轮次标记为中断:追加一条 LocalOnly assistant 消息
// (绝不发送给模型),记录未完成的工具调用与已输出的文本,供续跑恢复。
func MarkInterrupted(s *Session, interruptedOutput string, pendingToolCallIDs []string) {
	s.Add(provider.Message{
		Role:      provider.RoleAssistant,
		Content:   interruptedOutput,
		LocalOnly: true,
		InterruptedTurn: &provider.InterruptedTurnRecovery{
			Pending:           true,
			InterruptedTools:  pendingToolCallIDs,
			DroppedPartialText: interruptedOutput != "",
		},
	})
}

// InterruptedTurnRecoveryMessage 生成续跑提示消息(注入轮尾,不破坏前缀缓存)。
// 语义(docs/02 §7.2):中断轮次的工具调用不得执行,模型应在恢复消息指导下继续。
func InterruptedTurnRecoveryMessage(interrupted *provider.InterruptedTurnRecovery) provider.Message {
	var b strings.Builder
	b.WriteString("【上轮被中断】你的上一轮输出在完成前被打断。\n")
	if interrupted != nil {
		if interrupted.DroppedPartialText {
			b.WriteString("已输出的文本已被丢弃,请重新组织语言。\n")
		}
		if len(interrupted.InterruptedTools) > 0 {
			b.WriteString("未完成的工具调用 ID(不要重复执行,直接说明意图): " +
				strings.Join(interrupted.InterruptedTools, ", ") + "\n")
		}
	}
	b.WriteString("请基于已有信息继续完成当前任务;如果上轮正在等待工具结果,请重新发起需要的调用。")
	return provider.Message{Role: provider.RoleUser, Content: b.String()}
}
