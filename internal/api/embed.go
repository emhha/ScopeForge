package api

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// webDist 是前端构建产物(docs/05 §1.1 单二进制:go:embed)。
// 构建:cd web && npm install && npm run build → 产物进入 web/dist。
// 产物缺失时(未构建前端)SPA 端点返回构建提示,其余 API 不受影响。
//
//go:embed all:webdist
var webDist embed.FS

//go:embed all:webdist/index.html
var webIndex []byte

// spaFS 是嵌入的前端文件系统(web/dist)。
var spaFS = func() fs.FS {
	sub, err := fs.Sub(webDist, "webdist")
	if err != nil {
		return nil
	}
	return sub
}()

// hasWebDist 前端产物是否存在(编译期由 embed 决定:目录空则 index.html 缺失)。
var hasWebDist = len(webIndex) > 0

// handleSPA 提供嵌入的 SPA 与前端资源;未匹配到文件时回退 index.html
// (Vue Router history 模式,§1.4)。
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if !hasWebDist || spaFS == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "api-ok",
			"note":   "前端未构建:cd web && npm install && npm run build(产物经 go:embed 打包)",
		})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	// 防路径穿越:只允许常规静态资源路径
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(spaFS, path)
	if err != nil {
		// SPA fallback:未匹配的路径回退 index.html(Vue Router history)
		data, err = fs.ReadFile(spaFS, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		path = "index.html"
	}
	ct := contentType(path)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// contentType 按扩展名推断 MIME。
func contentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(path, ".map"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

var _ = fmt.Sprintf
