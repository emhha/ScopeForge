package server

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

const maxLedgerSkillName = 128

func recordNativeSkill(ctx context.Context, store *db.DB, key string, args *tool.BeforeToolArgs, found bool) {
	if args.ToolName != "skill_load" || store == nil {
		return
	}
	var in struct {
		Name string `json:"skill"`
	}
	if json.Unmarshal(args.Arguments, &in) != nil {
		return
	}
	name := strings.TrimSpace(in.Name)
	if len(name) > maxLedgerSkillName {
		name = name[:maxLedgerSkillName]
	}
	ri := agent.RunInfoFrom(ctx)
	err := store.InsertSkillUsage(&db.SkillUsage{Skill: name, AgentKey: key, TaskID: ri.TaskID, ExplorationID: ri.ExplorationID, IntentID: ri.IntentID, SessionID: ri.SessionID, Found: found})
	if err != nil {
		log.Printf("[skillusage] %v", err)
	}
}
