package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/config"
	"scopeforge/internal/event"
)

// ------------------------------------------------------------------ challenges

// handleChallenges 任务列表页数据源(§1.2):已运行任务卡 + 成本。
// 阶段 2.11(阶段二取代阶段一):平台抽象层删除,challenge 列表改从
// vulnerability_ledger 的 distinct challenge_id 获取(任务入口是配置注入,
// 非平台题单)。
func (s *Server) handleChallenges(w http.ResponseWriter, r *http.Request) {
	type card struct {
		ID        string  `json:"id"`
		CostUSD   float64 `json:"cost_usd"`
		Turns     int     `json:"turns"`
		UpdatedAt int64   `json:"updated_at"`
	}
	out := []card{}
	if s.deps.DB != nil {
		rows, err := s.deps.DB.Query(`SELECT DISTINCT challenge_id FROM vulnerability_ledger ORDER BY challenge_id`)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				c := card{ID: id}
				if s.deps.Ledger != nil {
					if sp, err := s.deps.Ledger.Spend(id); err == nil {
						c.CostUSD = sp.CostUSD
						c.Turns = sp.Turns
					}
				}
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"challenges": out})
}

// handleChallengeDetail 任务详情数据源(§2):facts/intents/workers/
// vulnerabilities(账本)/cells(覆盖矩阵)/interest(goal 高亮)/成本。
func (s *Server) handleChallengeDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	payload := map[string]any{"challenge_id": id}
	if s.deps.Board != nil {
		facts, _ := s.deps.Board.Facts(id, 0)
		intents, _ := s.deps.Board.Intents(id, nil, 0)
		workers, _ := s.deps.Board.Workers(id)
		vulns, _ := s.deps.Board.Vulnerabilities(id)
		payload["facts"] = facts
		payload["intents"] = intents
		payload["workers"] = workers
		// 漏洞账本(看板"进行中=submitted 等回执 / 已完成=accepted / 归档=其余"列数据源)
		payload["vulnerabilities"] = vulns
	}
	// 覆盖矩阵格子(看板"计划=open / 归档=dead|skipped"列数据源)
	if s.deps.DB != nil {
		type cell struct {
			CWE        string `json:"cwe"`
			Asset      string `json:"asset"`
			Endpoint   string `json:"endpoint"`
			Status     string `json:"status"`
			Form       string `json:"form"`
			SkipReason string `json:"skip_reason"`
			UpdatedAt  int64  `json:"updated_at"`
		}
		rows, err := s.deps.DB.Query(`SELECT COALESCE(cwe,''), asset, COALESCE(endpoint,''), status, COALESCE(form,''), COALESCE(skip_reason,''), updated_at
			FROM coverage_matrix WHERE challenge_id = ? ORDER BY asset, endpoint, cwe`, id)
		if err == nil {
			cells := []cell{}
			for rows.Next() {
				var c cell
				if rows.Scan(&c.CWE, &c.Asset, &c.Endpoint, &c.Status, &c.Form, &c.SkipReason, &c.UpdatedAt) == nil {
					cells = append(cells, c)
				}
			}
			rows.Close()
			payload["cells"] = cells
		} else {
			rows.Close()
		}
	}
	// goal 高亮数据(04 §5.2 interest:命中客户关注 CWE/严重级的发现高亮"完成有意义")
	if s.deps.Reports != nil {
		payload["interest"] = s.deps.Reports.Interest
	}
	if s.deps.Ledger != nil {
		if sp, err := s.deps.Ledger.Spend(id); err == nil {
			payload["spend"] = sp
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleBoard 黑板侧栏数据源(§2.2 紧凑快照)。
func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.deps.Board == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "board unavailable"})
		return
	}
	snap, err := s.deps.Board.SnapshotForWorker(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleEvidence 证据溯源(§2.2):ref=ev-<seq>(事件)或 flow-<n>(流量)。
// 返回该证据的完整原文(权威全文服务端拉取)。
func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ref := chi.URLParam(r, "ref")
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	evs, _ := event.QueryForChallenge(s.deps.DB, id, 0)
	var found *event.Event
	for i := range evs {
		if refOf(evs[i]) == ref {
			found = &evs[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "evidence not found", "ref": ref})
		return
	}
	// 证据的支撑事实(该事件对应的黑板 fact)
	payload := map[string]any{"ref": ref, "event": found}
	if s.deps.Board != nil {
		if facts, err := s.deps.Board.Facts(id, 0); err == nil {
			supporting := []blackboard.Fact{} // 空数组而非 null(前端渲染)
			for _, f := range facts {
				if f.EvidenceRef == ref {
					supporting = append(supporting, f)
				}
			}
			payload["supporting_facts"] = supporting
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// refOf 从事件提取引用标识(ev-<seq> 或 traffic 的 flow_ref)。
func refOf(e event.Event) string {
	if e.Kind == event.KindTraffic {
		if p, ok := e.Payload.(map[string]any); ok {
			if fr, _ := p["flow_ref"].(string); fr != "" {
				return fr
			}
		}
	}
	return fmt.Sprintf("ev-%d", e.Seq)
}

// ------------------------------------------------------------------ ledger/system

// handleLedger 账本页(§2.4):按 role×model 统计 token/美元。
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db unavailable"})
		return
	}
	rows, err := s.deps.DB.Query(`SELECT role, model, SUM(prompt_tokens), SUM(cache_hit_tokens),
		SUM(output_tokens), SUM(reasoning_tokens), SUM(cost_usd), COUNT(*) FROM ledger
		GROUP BY role, model ORDER BY cost_usd DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	type row struct {
		Role     string  `json:"role"`
		Model    string  `json:"model"`
		Prompt   int64   `json:"prompt_tokens"`
		CacheHit int64   `json:"cache_hit_tokens"`
		Output   int64   `json:"output_tokens"`
		Reason   int64   `json:"reasoning_tokens"`
		CostUSD  float64 `json:"cost_usd"`
		Calls    int64   `json:"calls"`
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Role, &r.Model, &r.Prompt, &r.CacheHit, &r.Output, &r.Reason, &r.CostUSD, &r.Calls); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out = append(out, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out})
}

// handleSystem 系统页(§1.2):provider 连通性/容器/隧道/监听器/动态池。
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{"ts": time.Now().Unix()}
	// provider 连通性(不实际调用,展示配置与注册状态)
	if s.deps.Providers != nil {
		var ps []map[string]any
		for name := range s.deps.Providers {
			ps = append(ps, map[string]any{"name": name, "available": true})
		}
		payload["providers"] = ps
	}
	if s.deps.Route != nil {
		payload["routes"] = s.deps.Route.State()
	}
	if s.deps.Listener != nil {
		payload["listeners"] = s.deps.Listener.State()
	}
	if s.deps.Sandbox != nil {
		payload["docker_available"] = s.deps.Sandbox.Available(r.Context())
		payload["sandbox_image"] = s.deps.Sandbox.Image
	}
	if s.deps.KB != nil {
		b, p := s.deps.KB.Count()
		payload["kb_entries"] = map[string]int{"builtin": b, "plugins": p}
	}
	writeJSON(w, http.StatusOK, payload)
}

// ------------------------------------------------------------------ config

// handleGetConfig 配置展示(YAML 文本,密钥脱敏,§1.2 配置页)。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Cfg
	// 密钥只显示存在性
	safe := cfg
	safe.Providers = nil
	for _, p := range cfg.Providers {
		cp := p
		if cp.APIKeyEnv != "" {
			cp.APIKeyEnv = "***"
		}
		safe.Providers = append(safe.Providers, cp)
	}
	data, err := yaml.Marshal(safe)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config_yaml": string(data)})
}

// handlePutConfig 配置校验 + 持久化(YAML body,与 scopeforge.yaml 同构,§1.2 全量 CRUD):
// 校验通过后写入配置文件路径(ConfigPath);热重载由 serve 重启生效。
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	var incoming config.Config
	if err := yaml.Unmarshal(body, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad yaml: " + err.Error()})
		return
	}
	if err := validateConfig(incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	path := s.deps.ConfigPath
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "config_path not configured (serve 启动时需 --config)"})
		return
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "write config: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "path": path, "note": "重启后生效"})
}

// ------------------------------------------------------------------ helpers

// authorized bearer token 校验(写操作/未脱敏数据)。
func (s *Server) authorized(r *http.Request) bool {
	if s.deps.AuthToken == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	return h == "Bearer "+s.deps.AuthToken
}

func validateConfig(c config.Config) error {
	for i, p := range c.Providers {
		if p.Name == "" || p.Kind == "" || p.BaseURL == "" || p.Model == "" {
			return fmt.Errorf("providers[%d] requires name/kind/base_url/model", i)
		}
		if p.Kind != "openai" && p.Kind != "anthropic" {
			return fmt.Errorf("providers[%d].kind %q unknown", i, p.Kind)
		}
	}
	switch c.Tools.Permissions {
	case "", "ask", "auto", "yolo":
	default:
		return fmt.Errorf("tools.permissions %q invalid", c.Tools.Permissions)
	}
	return nil
}
