package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/emhha/scopeforge/toolkit"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/learning"
	"github.com/emhha/scopeforge/skillcatalog"
	"github.com/emhha/scopeforge/traffic"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

// wireAgentAugment connects the PG agent_visibility table into the agent runtime:
// an agent's visible skills are loaded from the filesystem and packed into one
// Skill meta-tool; its visible stdio MCP servers are spawned and expanded to
// mcp__server__tool. skillDir is the root directory of all skill subdirectories.
// hostTools, if set, returns runtime host tools (currently the traffic tools when
// capture is on) to add to EVERY agent's base list — the DB tools table then
// filters them per-agent binding. Empty/nil → no host tools this run (capture off).
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CallableTool, map[string][]string), memories *learning.Memories, kb *learning.Knowledge) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CallableTool, agent.AugmentInfo, func()) {
		role, err := pg.GetAgentByKey(agentKey)
		if err != nil || role == nil {
			return nil, agent.AugmentInfo{}, nil
		}
		var extra []actool.CallableTool
		var closers []io.Closer
		var opts []llmagent.Option
		var taskMemory memory.Service
		if memories != nil {
			settings := memories.Settings()
			if settings.GlobalEnabled {
				extra = append(extra, memories.GlobalSearchTool())
			}
			if ri := agent.RunInfoFrom(ctx); ri.TaskID > 0 && settings.TaskEnabled {
				taskMemory = memories.Task(ri.TaskID)
				for _, t := range taskMemory.Tools() {
					if c, ok := t.(actool.CallableTool); ok {
						extra = append(extra, c)
					}
				}
			}
		}
		var nativeKnowledge knowledge.Knowledge
		if kb != nil {
			nativeKnowledge = kb
		}
		visible, _ := pg.AgentSkillNames(role.ID)
		allowed := map[string]bool{}
		for _, n := range visible {
			allowed[n] = true
		}
		catalog, err := skillcatalog.Load(skillDir)
		if err != nil {
			log.Printf("[skills] %v", err)
		}
		gated := map[string][]string{}
		if catalog != nil {
			catalog = catalog.Filter(allowed)
			if len(catalog.List()) > 0 {
				opts = append(opts, llmagent.WithSkills(catalog.Repository()), llmagent.WithSkillToolProfile(llmagent.SkillToolProfileKnowledgeOnly))
			}
			for _, entry := range catalog.List() {
				for _, server := range entry.MCPs {
					gated[server] = append(gated[server], entry.Name)
				}
			}
		}
		ids, _ := pg.AgentVisible(role.ID, "mcp")
		direct := idSet(ids)
		servers, _ := pg.ListMCP()
		for _, m := range servers {
			if !m.Enabled || (!direct[m.ID] && len(gated[m.Name]) == 0) {
				continue
			}
			client, err := connectMCP(ctx, m)
			if err != nil {
				log.Printf("[mcp] %s: %v", m.Name, err)
				continue
			}
			closers = append(closers, client)
			tools, err := client.Tools(ctx)
			if err != nil {
				log.Printf("[mcp] %s: %v", m.Name, err)
				continue
			}
			if len(gated[m.Name]) == 0 {
				extra = append(extra, tools...)
			} else {
				set := &nativeToolSet{name: m.Name}
				for _, t := range tools {
					set.tools = append(set.tools, t)
				}
				opts = append(opts, llmagent.WithToolSets([]actool.ToolSet{set}))
				for _, skillName := range gated[m.Name] {
					opts = append(opts, llmagent.WithToolActivationOnSkillLoad(skillName, []string{m.Name}))
				}
			}
		}
		if hostTools != nil {
			tools, _ := hostTools()
			extra = append(extra, tools...)
		}
		callbacks := actool.NewCallbacks().RegisterAfterTool(func(ctx context.Context, args *actool.AfterToolArgs) (*actool.AfterToolResult, error) {
			recordNativeSkill(ctx, pg, agentKey, &actool.BeforeToolArgs{ToolName: args.ToolName, Arguments: args.Arguments}, args.Error == nil)
			return nil, nil
		})
		return extra, agent.AugmentInfo{Memory: taskMemory, Knowledge: nativeKnowledge, AgentOptions: opts, Callbacks: callbacks, InteractiveShell: role.InteractiveShell}, func() {
			for _, c := range closers {
				_ = c.Close()
			}
		}
	}
}

type nativeToolSet struct {
	name  string
	tools []actool.Tool
}

func (s *nativeToolSet) Name() string                        { return s.name }
func (s *nativeToolSet) Tools(context.Context) []actool.Tool { return s.tools }
func (s *nativeToolSet) Close() error                        { return nil }

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// seedPrompts writes each built-in agent's code-default prompt body into
// agent_prompts on startup — first-insert only (SeedPromptIfEmpty is a no-op once
// any version exists), so the DB becomes the authoritative editable source while
// user edits survive restarts. Runs after seedBuiltins has created the agent rows.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 跳过: agent 不存在 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 失败: %v", key, err)
		}
	}
}

// wireTools seeds the built-in tool catalog (idempotent, first-insert only so page
// edits survive restart) and wires the DB tools table into the agent runtime: at
// tool-assembly time each built-in tool is filtered by its agent binding / enabled
// flag and, if kept, wrapped so the model sees the DB-overridden description/schema
// and缺省入参 get injected. MCP/skill/host tools have no row and pass through.
func wireTools(pg *db.DB, domainReg map[string]actool.CallableTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	// Seed the built-in domain tools (first-insert only; DO NOTHING preserves edits).
	// No startup prune: rows we didn't seed are left alone so future user-defined
	// custom tools (system=false, added via the UI) survive restarts.
	for _, s := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(s.Schema)
		agents, _ := json.Marshal(s.Agents)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", s.Key, err)
		}
	}
	// Seed the traffic host tools so they're bindable per-agent like built-ins.
	// Default binding = worker (preserves prior behavior). Their runtime availability
	// is still gated by the global capture switch (hostTools() returns them only when
	// capture is on), so an off-capture binding simply never surfaces the tool.
	trafficAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range traffic.SeedToolMetas() {
		schema, _ := json.Marshal(toolkit.SchemaMap(t.Declaration().InputSchema))
		if err := pg.SeedTool(t.Declaration().Name, t.Declaration().Description, schema, trafficAgents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Declaration().Name, err)
		}
	}
	// bashInteractiveShellNote is appended to Bash's description ONLY for agents whose
	// interactive_shell is on, so Bash points at shell_open for interactive programs
	// without ever referencing a tool that isn't injected (§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n需要【交互输入】的程序（msfconsole / ssh 交互登录 / mysql、psql、python 等 REPL / 密码或 yes/no 提示 / nc 反弹 shell）不要用 Bash（它没有 stdin、会卡住），改用 shell_open 开交互会话（用完 shell_close）。一次性、非交互命令仍用 Bash。"
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CallableTool) []actool.CallableTool {
		rows, err := pg.ListTools()
		if err != nil {
			log.Printf("[tools] 读取工具表失败，按代码默认放行: %v", err)
			return tools
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CallableTool, row *db.Tool) actool.CallableTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		out := tools[:0:0]
		for _, t := range tools {
			row, known := byKey[t.Declaration().Name]
			if !known { // MCP/skill/host tool: no row → untouched
				out = append(out, t)
				continue
			}
			if !row.Enabled || !contains(row.Agents, agentKey) {
				continue // disabled globally or not bound to this agent → drop
			}
			out = append(out, resolve(t, row))
		}
		// inject: domain tools bound to this agent in the DB but absent from the
		// incoming list. Covers agents (Auto, custom) whose base only has DefaultTools
		// and therefore never includes ToolSet-backed domain tools. Per-task instances
		// in the base always win: inList is built from the original incoming list so a
		// worker's own upsert_asset is never shadowed by the server-level registry copy.
		if len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Declaration().Name] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || !contains(row.Agents, agentKey) || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // not a domain tool; custom/host tools are injected via hostTools()
				}
				out = append(out, resolve(inst, row))
			}
		}
		// shell hints: user-defined kind="shell" tools are not callable — they are
		// environment declarations that tell the model which command-line tools are
		// installed. Collect the ones bound to this agent and append to Bash's description.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled && contains(row.Agents, agentKey) {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n以下工具已安装在此 bash 环境中，可直接通过 Bash 调用：\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Declaration().Name == "Bash" {
					out[i] = agent.DecorateTool(t, t.Declaration().Description+note, toolkit.SchemaMap(t.Declaration().InputSchema))
					break
				}
			}
		}
		// interactive shell: gated purely by the agent's interactive_shell flag (like
		// web_search), NOT by tools-table binding. When on, inject the 5 shell_* tools
		// and COUPLE the Bash description addendum so it points at shell_open — and never
		// dangles when off. See docs/交互式shell设计.md §14.2.

		return out
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg builds a name→CoreTool registry from a server-level ToolSet
// (real AssetStore, nil ExplorationStore, taskID=0). Used by ToolResolve to inject
// domain tools into agents (Auto, custom) that don't own a per-task ToolSet.
// nil as → returns nil (no injection, graceful degradation).
//
// The nil ExplorationStore is deliberate — these instances are task-less by
// construction — so every tool here must tolerate it. Asset/company tools do
// (they only need the AssetStore); the exploration-graph tools refuse with a
// clear message via ToolSet.needExploration. Binding one of them to a task-less
// agent in the tools table is therefore a useless tool, not a crash.
func buildDomainReg(as *db.AssetStore) map[string]actool.CallableTool {
	if as == nil {
		return nil
	}
	serverTS := agent.NewToolSet(nil, "")
	serverTS.SetAssetStore(as, as.Companies())
	reg := make(map[string]actool.CallableTool)
	for _, t := range serverTS.AllDomainTools() {
		reg[t.Declaration().Name] = t
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
