package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	sdkhttp "github.com/emhha/netloom/nethttp"
	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/discovery"
	"github.com/emhha/scopeforge/netrequest"
	"github.com/emhha/scopeforge/toolkit"
	"github.com/emhha/scopeforge/traffic"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func (s *Server) networkTools() []tool.CallableTool {
	httpSchema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"url"}, "properties": map[string]any{
		"url": strParam("HTTP/HTTPS 目标 URL"), "method": strParam("HTTP 方法；默认 GET"),
		"headers":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		"header_values": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		"body":          strParam("请求正文"), "body_encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}},
		"query": nameValueSchema(), "form": nameValueSchema(),
		"multipart":        map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "value"}, "properties": map[string]any{"name": strParam("字段名"), "value": strParam("字段值或文件内容"), "filename": strParam("可选文件名"), "content_type": strParam("可选 MIME 类型"), "encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}}}}},
		"fresh_connection": map[string]any{"type": "boolean"},
		"basic_auth":       map[string]any{"type": "object", "additionalProperties": false, "required": []string{"username", "password"}, "properties": map[string]any{"username": strParam("用户名"), "password": strParam("密码")}},
		"timeout_seconds":  map[string]any{"type": "integer", "minimum": 0, "maximum": 120},
		"follow_redirects": map[string]any{"type": "boolean"}, "max_redirects": map[string]any{"type": "integer", "minimum": 0, "maximum": 20},
		"insecure_tls": map[string]any{"type": "boolean", "description": "显式跳过目标证书校验；默认校验"},
		"http_version": map[string]any{"type": "string", "enum": []string{"auto", "1.1", "2", "2-only"}},
		"verbose":      map[string]any{"type": "boolean", "description": "默认 false；设为 true 时额外显示 curl 风格的每跳请求行、请求头、响应头、连接 IP 和 TLS 信息"},
	}}
	return []tool.CallableTool{
		toolkit.New("http_request", "使用 netloom 发送 HTTP 请求并通过原生 passive 返回逐跳被动分析结果，支持方法、重复头、查询、表单、正文、认证、TLS 和重定向。输出为一行精简 JSON 元信息、空行、实际正文；verbose=true 在正文前增加 curl 风格的每跳请求/响应头。遵循全局出口代理及每跳资产规则。流量捕获开启时返回 exchange_ids。正文展示最多 16 KiB，采集最多 8 MiB；EOF 等读取错误仍返回已收到的正文，complete=false 表示采集不完整。", httpSchema, func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
			failure := func(err error) (toolkit.Output, error) {
				return toolkit.Error(netrequest.Format(nil, err, false)), nil
			}
			if s.m.httpRequests == nil {
				return failure(errors.New("HTTP 请求服务未就绪"))
			}
			var input struct {
				sdkhttp.RequestInput
				Verbose bool `json:"verbose"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return failure(err)
			}
			task := agent.RunInfoFrom(ctx).TaskID
			var store *traffic.Traffic
			if s.m.TrafficEnabled() {
				store = s.m.Traffic()
			}
			out, err := s.m.httpRequests.Do(ctx, input.RequestInput, store, func(ctx context.Context, target string) error { return s.validateHTTPURL(ctx, task, target) })
			return toolkit.Output{Text: netrequest.Format(out, err, input.Verbose), IsError: err != nil}, nil
		}),
		toolkit.New("list_recon_sources", "列出已启用的 FOFA/Hunter/Quake 测绘源及 API Key 配置状态，不返回凭据。", objSchema(map[string]any{}), func(context.Context, json.RawMessage) (toolkit.Output, error) {
			if s.m.mapping == nil {
				return toolkit.Error("测绘服务未就绪"), nil
			}
			views, err := s.m.mapping.Sources()
			if err != nil {
				return toolkit.Error("读取测绘配置失败"), nil
			}
			out := []map[string]any{}
			for _, v := range views {
				out = append(out, map[string]any{"id": v.ID, "enabled": v.Enabled, "configured": v.KeySet})
			}
			return jsonResult(map[string]any{"sources": out})
		}),
		toolkit.New("search_mapping", "使用 netloom 的 FOFA/Hunter/Quake 客户端查询网络测绘资产。query 为域名或 IP，单源可用 native 原生语法；all 合并去重结果。API Key 由系统配置托管。结果仅为候选，确认后使用 insert_assets 登记。", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"query"}, "properties": map[string]any{"query": strParam("域名、IP 或数据源原生语法"), "provider": map[string]any{"type": "string", "enum": []string{"all", "fofa", "hunter", "quake"}}, "native": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}}}, func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
			if s.m.mapping == nil {
				return toolkit.Error("测绘服务未就绪"), nil
			}
			var input discovery.Input
			if err := json.Unmarshal(raw, &input); err != nil {
				return toolkit.Error(err.Error()), nil
			}
			out, err := s.m.mapping.Search(ctx, input)
			if err != nil {
				return toolkit.Error(err.Error()), nil
			}
			return jsonResult(out)
		}),
	}
}
func nameValueSchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "value"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}}}}
}
func (s *Server) validateHTTPURL(ctx context.Context, task int64, raw string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	blocks, err := s.m.pg.ListAssetInterceptRules()
	if err != nil {
		return err
	}
	var allows []db.AssetInterceptRule
	if task > 0 {
		pt, e := s.m.pg.GetTask(task)
		if e != nil {
			return e
		}
		if pt == nil {
			return errors.New("任务不存在")
		}
		b, a, e := s.m.assets.TaskInterceptRulesSplit(task)
		if e != nil {
			return e
		}
		blocks = append(blocks, b...)
		allows = a
	}
	var domains, ips []string
	if net.ParseIP(u.Hostname()) != nil {
		ips = []string{u.Hostname()}
	} else {
		domains = []string{u.Hostname()}
	}
	if decision := db.EvaluateAssetGate(blocks, allows, domains, ips, []string{raw}); !decision.Allowed {
		return fmt.Errorf("HTTP 目标被资产规则拒绝：%s", decision.Reason)
	}
	return nil
}
func (s *Server) registerNetworkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/discovery/sources", func(w http.ResponseWriter, r *http.Request) {
		if s.m.mapping == nil {
			writeErr(w, 503, "测绘服务未就绪")
			return
		}
		views, err := s.m.mapping.Sources()
		if err != nil {
			writeErr(w, 500, "读取配置失败")
			return
		}
		writeJSON(w, 200, map[string]any{"sources": views})
	})
	mux.HandleFunc("PUT /api/discovery/sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.m.mapping == nil {
			writeErr(w, 503, "测绘服务未就绪")
			return
		}
		var patch discovery.Patch
		r.Body = http.MaxBytesReader(w, r.Body, 32768)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&patch); err != nil {
			writeErr(w, 400, "配置格式无效")
			return
		}
		view, err := s.m.mapping.Update(r.PathValue("id"), patch)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, view)
	})
}
