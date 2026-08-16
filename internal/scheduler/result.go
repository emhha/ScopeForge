package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scopeforge/internal/blackboard"
)

// buildResult 汇总报告数据。
func (s *Scheduler) buildResult(challengeID string) *Result {
	s.mu.Lock()
	reason := s.termReason
	concluded := s.concluded
	s.mu.Unlock()
	res := &Result{
		ChallengeID: challengeID,
		Concluded:   concluded,
		Reason:      reason,
		Terminated:  concluded,
	}
	res.Facts, _ = s.board.Facts(challengeID, 0)
	res.Vulns, _ = s.board.Vulnerabilities(challengeID)
	if s.ledger != nil {
		spend, _ := s.ledger.Spend(challengeID)
		res.CostUSD = spend.CostUSD
		res.Turns = spend.Turns
	}
	// 报告文件(M1:结构化 JSON;M3 升级正式报告)
	if concluded {
		if path, err := s.writeReport(challengeID, res); err == nil {
			res.ReportPath = path
		}
	}
	return res
}

// writeReport 写结构化报告 JSON 到 reportDir。
func (s *Scheduler) writeReport(challengeID string, res *Result) (string, error) {
	if err := os.MkdirAll(s.reportDir, 0o755); err != nil {
		return "", err
	}
	report := map[string]any{
		"challenge_id":    challengeID,
		"terminated":      res.Terminated,
		"reason":          string(res.Reason),
		"facts":           res.Facts,
		"vulnerabilities": res.Vulns, // phase2 漏洞账本
		"cost_usd":        res.CostUSD,
		"turns":           res.Turns,
		"generated_at":    time.Now().Unix(),
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	path := filepath.Join(s.reportDir, challengeID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// snapshotText 生成快照 YAML 增量文本。
func (s *Scheduler) snapshotText(challengeID string) string {
	snap, err := s.board.SnapshotForWorker(challengeID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "challenge: %s\nas_of_seq: %d\n", challengeID, snap.AsOfSeq)
	b.WriteString("facts:\n")
	for _, f := range snap.Facts {
		fmt.Fprintf(&b, "- [%s] %s (weight %.2f, %s)\n", f.Prefix, f.Text, f.Weight, f.State)
	}
	b.WriteString("intents:\n")
	for _, it := range snap.Intents {
		fmt.Fprintf(&b, "- %s (weight %.2f, %s)\n", it.Text, it.Weight, it.State)
	}
	b.WriteString("\n")
	return b.String()
}

// completedSummary 汇总已确认事实(防重复)。
func (s *Scheduler) completedSummary(challengeID string) string {
	facts, err := s.board.Facts(challengeID, 10)
	if err != nil || len(facts) == 0 {
		return "(无)"
	}
	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Prefix, f.Text)
	}
	return b.String()
}

// forbiddenSummary 列出死路(禁止重复)。
func (s *Scheduler) forbiddenSummary(challengeID string) string {
	facts, err := s.board.Facts(challengeID, 0)
	if err != nil {
		return ""
	}
	var dead []string
	for _, f := range facts {
		if f.Prefix == blackboard.PrefixDead {
			dead = append(dead, f.Text)
		}
	}
	if len(dead) == 0 {
		return ""
	}
	return "dead_ends: " + strings.Join(dead, "; ")
}
