// Package runstate describes product-visible run outcomes, independent of SDK events.
package runstate

import "trpc.group/trpc-go/trpc-agent-go/model"

type Reason string

const (
	ReasonCompleted         Reason = "completed"
	ReasonMaxTurns          Reason = "max_turns"
	ReasonTimeout           Reason = "timeout"
	ReasonModelError        Reason = "model_error"
	ReasonBlockingLimit     Reason = "blocking_limit"
	ReasonPromptTooLong     Reason = "prompt_too_long"
	ReasonImageError        Reason = "image_error"
	ReasonStopHookPrevented Reason = "stop_hook_prevented"
	ReasonHookStopped       Reason = "hook_stopped"
	ReasonAbortedStreaming  Reason = "aborted_streaming"
	ReasonAbortedTools      Reason = "aborted_tools"
)

type Outcome struct {
	Reason Reason
	Text   string
	Err    error
	Turns  int
	Usage  model.Usage
}
type Wrapup struct {
	Prompt         string
	DisabledTools  []string
	MaxTurns       int
	PromptByReason map[Reason]string
}
