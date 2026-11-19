package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// NodeSource is internal provenance authored only by domain write handlers.
type NodeSource struct {
	NodeID   int64
	Relation string
}

type LineageContribution struct {
	NodeID    string `json:"node_id"`
	Relation  string `json:"relation"`
	Summary   string `json:"summary"`
	Seq       int64  `json:"seq,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Precision string `json:"precision"`
}

type LineageSession struct {
	TaskID        string                `json:"task_id"`
	Session       string                `json:"session"`
	Title         string                `json:"title"`
	Role          string                `json:"role"`
	Contributions []LineageContribution `json:"contributions"`
}

// FindingLineageSessions never infers an actor from a node's origin/owner or a
// reusable executor name. Event provenance is authoritative; historical yields
// is only a logical association, not evidence of who performed a verification.
func (s *ExplorationStore) FindingLineageSessions(nodeID int64) ([]LineageSession, error) {
	out := []LineageSession{}
	nodes, edges, err := s.FindingLineage(nodeID)
	if err != nil || len(nodes) == 0 {
		return out, err
	}
	var taskID int64
	err = s.db.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, s.expID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	byNode := make(map[int64]*Node, len(nodes))
	for _, n := range nodes {
		byNode[n.ID] = n
	}
	sessions := map[string]*LineageSession{}
	add := func(key, role, title string, c LineageContribution) {
		item := sessions[key]
		if item == nil {
			item = &LineageSession{TaskID: strconv.FormatInt(taskID, 10), Session: key, Title: title, Role: role, Contributions: []LineageContribution{}}
			sessions[key] = item
		}
		for _, prior := range item.Contributions {
			if prior.NodeID == c.NodeID && prior.Relation == c.Relation && prior.Seq == c.Seq && prior.Precision == c.Precision {
				return
			}
		}
		item.Contributions = append(item.Contributions, c)
	}
	summary := func(n *Node) string {
		var payload struct {
			Summary string `json:"summary"`
			Text    string `json:"text"`
		}
		_ = json.Unmarshal(n.Payload, &payload)
		if payload.Summary != "" {
			return payload.Summary
		}
		if payload.Text != "" {
			return payload.Text
		}
		return fmt.Sprintf("%s #%d", n.Kind, n.ID)
	}
	// Only actual node_sources links receive an exact activity deep-link.
	// Keep the same causal scope as FindingLineage; covers is not ancestry.
	rows, err := s.db.Query(`WITH RECURSIVE anc(id) AS (
 SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND id=$2
 UNION SELECT e.src_id FROM exploration_edges e JOIN anc ON e.dst_id=anc.id WHERE e.exploration_id=$1 AND e.rel<>'covers'
 )
 SELECT ns.node_id, ns.relation, a.id, COALESCE(a.tool_use_id,''), COALESCE(a.worker,''), COALESCE(a.main_seg,0), COALESCE(n.id,0), COALESCE(n.payload,'{}')
 FROM node_sources ns
 JOIN activity a ON a.id=ns.activity_id AND a.exploration_id=$1
 JOIN exploration_nodes output ON output.id=ns.node_id AND output.exploration_id=$1
 LEFT JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=$1 AND n.kind='intent'
 WHERE ns.node_id IN (SELECT id FROM anc) ORDER BY a.id, ns.node_id, ns.relation`, s.expID, nodeID)
	if err != nil {
		return nil, err
	}
	exact := map[string]bool{}
	for rows.Next() {
		var nid, seq, intent int64
		var seg int
		var relation, call, worker, payload string
		if err = rows.Scan(&nid, &relation, &seq, &call, &worker, &seg, &intent, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		key, role, title := "", "", ""
		switch {
		case worker == "mainagent":
			key, role, title = fmt.Sprintf("main:%d", seg), "mainagent", fmt.Sprintf("主会话 #%d", seg+1)
		case worker == "planner":
			key, role, title = "plan", "planner", "规划会话"
		case intent > 0:
			key, role, title = fmt.Sprintf("intent:%d", intent), "worker", summary(&Node{ID: intent, Kind: KindIntent, Payload: json.RawMessage(payload)})
		}
		if key == "" {
			continue
		}
		add(key, role, title, LineageContribution{NodeID: strconv.FormatInt(nid, 10), Relation: relation, Summary: summary(byNode[nid]), Seq: seq, ToolUseID: call, Precision: "event"})
		exact[fmt.Sprintf("%d:%s", nid, relation)] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Historical activity.node_id identifies the executed intent, not its output.
	rows, err = s.db.Query(`SELECT DISTINCT n.id FROM activity a JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id AND n.kind='intent' WHERE a.exploration_id=$1 AND COALESCE(a.worker,'') NOT IN ('mainagent','planner') AND a.kind IN ('tool_use','tool_result','model_call','text','thinking','usage','result') ORDER BY n.id`, s.expID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		n := byNode[id]
		if n == nil {
			continue
		}
		add(fmt.Sprintf("intent:%d", id), "worker", summary(n), LineageContribution{NodeID: strconv.FormatInt(id, 10), Relation: "executed", Summary: summary(n), Precision: "session"})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		from, to := byNode[e.From], byNode[e.To]
		if e.Rel != RelYields || from == nil || from.Kind != KindIntent || to == nil || (to.Kind != KindFact && to.Kind != KindFinding) || exact[fmt.Sprintf("%d:produced", to.ID)] {
			continue
		}
		add(fmt.Sprintf("intent:%d", from.ID), "worker", summary(from), LineageContribution{NodeID: strconv.FormatInt(to.ID, 10), Relation: "produced", Summary: "历史 yields 关联（非精确执行者）：" + summary(to), Precision: "legacy"})
	}
	keys := make([]string, 0, len(sessions))
	for key := range sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, *sessions[key])
	}
	return out, nil
}
