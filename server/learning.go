package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emhha/scopeforge/internal/semantic"
	"github.com/emhha/scopeforge/learning"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
)

func (s *Server) registerLearningRoutes(mux *http.ServeMux) {
	s.registerKnowledgeImportRoutes(mux)
	mux.HandleFunc("GET /api/retrieval/config", s.retrievalConfig)
	mux.HandleFunc("PUT /api/retrieval/config", s.saveRetrievalConfig)
	mux.HandleFunc("POST /api/retrieval/test", s.testRetrievalConfig)
	mux.HandleFunc("GET /api/retrieval/status", s.retrievalStatus)
	mux.HandleFunc("GET /api/retrieval/component", s.semanticComponent)
	mux.HandleFunc("POST /api/retrieval/component/install", s.installSemanticComponent)
	mux.HandleFunc("POST /api/retrieval/component/import", s.importSemanticComponent)

	mux.HandleFunc("GET /api/knowledge/component", s.semanticComponent)
	mux.HandleFunc("POST /api/knowledge/component/install", s.installSemanticComponent)
	mux.HandleFunc("POST /api/knowledge/component/import", s.importSemanticComponent)
	mux.HandleFunc("GET /api/knowledge/config", s.retrievalConfig)
	mux.HandleFunc("PUT /api/knowledge/config", s.saveRetrievalConfig)
	mux.HandleFunc("POST /api/knowledge/test", s.testRetrievalConfig)
	mux.HandleFunc("GET /api/knowledge/status", s.knowledgeStatus)
	mux.HandleFunc("GET /api/memories/config", s.memoryConfig)
	mux.HandleFunc("PUT /api/memories/config", s.saveMemoryConfig)
	mux.HandleFunc("GET /api/memories/status", s.memoryStatus)
	mux.HandleFunc("GET /api/knowledge/documents", s.knowledgeDocuments)
	mux.HandleFunc("POST /api/knowledge/documents", s.uploadKnowledgeDocument)
	mux.HandleFunc("POST /api/knowledge/documents/{doc}/retry", s.retryKnowledgeDocument)
	mux.HandleFunc("DELETE /api/knowledge/documents/{doc}", s.deleteKnowledgeDocument)
	mux.HandleFunc("POST /api/knowledge/search", s.searchKnowledge)
	mux.HandleFunc("GET /api/memories/global", s.globalMemories)
	mux.HandleFunc("DELETE /api/memories/global/{memory}", s.deleteGlobalMemory)
	mux.HandleFunc("GET /api/tasks/{id}/memories", s.taskMemories)
	mux.HandleFunc("POST /api/tasks/{id}/memories", s.addTaskMemory)
	mux.HandleFunc("DELETE /api/tasks/{id}/memories/{memory}", s.deleteTaskMemory)
	mux.HandleFunc("POST /api/tasks/{id}/memories/distill", s.distillTaskMemory)
}

func (s *Server) semanticComponent(w http.ResponseWriter, r *http.Request) {
	status := s.m.retrieval.Components().Status()
	client, _ := s.m.retrieval.SemanticIndex().Runtime()
	loaded := client != nil && client.ModelLoaded()
	writeJSON(w, 200, struct {
		semantic.ComponentStatus
		ModelLoaded bool   `json:"model_loaded"`
		Mode        string `json:"mode"`
	}{status, loaded, s.m.retrieval.Config().Mode})
}
func (s *Server) installSemanticComponent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var req struct {
		Local bool `json:"local"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "无效安装请求")
		return
	}
	if s.m.retrieval.Config().Mode != "none" {
		writeErr(w, 409, "安装或升级前请先切换为关键词检索")
		return
	}
	if err := s.m.retrieval.Components().Install(req.Local, ""); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, s.m.retrieval.Components().Status())
}
func (s *Server) importSemanticComponent(w http.ResponseWriter, r *http.Request) {
	if s.m.retrieval.Config().Mode != "none" {
		writeErr(w, 409, "安装或升级前请先切换为关键词检索")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, semantic.MaxBundleBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, 400, "请上传小于 1 GiB 的组件 ZIP 包")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少安装包")
		return
	}
	defer f.Close()
	if header.Size > semantic.MaxBundleBytes {
		writeErr(w, 400, "安装包超过 1 GiB")
		return
	}
	stage, err := os.CreateTemp("", "scopeforge-semantic-*.zip")
	if err != nil {
		writeErr(w, 500, "无法创建临时文件")
		return
	}
	n, err := io.Copy(stage, io.LimitReader(f, semantic.MaxBundleBytes+1))
	closeErr := stage.Close()
	if err != nil || closeErr != nil || n > semantic.MaxBundleBytes {
		os.Remove(stage.Name())
		writeErr(w, 400, "安装包读取失败或超过限制")
		return
	}
	if err = s.m.retrieval.Components().Install(r.FormValue("local") != "false", stage.Name()); err != nil {
		os.Remove(stage.Name())
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, s.m.retrieval.Components().Status())
}
func (s *Server) knowledgeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.knowledge.Status())
}
func (s *Server) testRetrievalConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var c learning.EmbeddingConfig
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, "无效配置")
		return
	}
	c, err := s.m.retrieval.TestConnection(r.Context(), c)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) memoryConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.memories.Settings())
}
func (s *Server) saveMemoryConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var c learning.MemorySettings
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, "无效配置")
		return
	}
	if err := s.m.memories.Configure(c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s.m.memories.Settings())
}
func (s *Server) memoryStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"settings": s.m.memories.Settings(), "retrieval": s.m.retrieval.Status(), "jobs": s.m.memories.JobStatus()})
}
func (s *Server) retrievalConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.retrieval.Config())
}
func (s *Server) saveRetrievalConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var c learning.EmbeddingConfig
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, "无效的配置")
		return
	}
	if err := s.m.retrieval.Configure(c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, s.m.retrieval.Config())
}
func (s *Server) knowledgeDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.m.pg.KnowledgeDocuments()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, docs)
}
func (s *Server) uploadKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, learning.MaxDocumentBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeErr(w, 400, "上传失败或文件超过 20 MiB")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "请选择文件")
		return
	}
	defer f.Close()
	doc, err := s.m.knowledge.Upload(h.Filename, f)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, doc)
}
func (s *Server) retryKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	if err := s.m.knowledge.Retry(r.PathValue("doc")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, 404, "文档不存在")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	writeJSON(w, 202, map[string]any{"queued": true})
}
func (s *Server) deleteKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	if err := s.m.knowledge.Delete(r.Context(), r.PathValue("doc")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, 404, "文档不存在")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	w.WriteHeader(204)
}
func (s *Server) searchKnowledge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req struct {
		Query string `json:"query"`
	}
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Query) == "" {
		writeErr(w, 400, "请输入检索问题")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := s.m.knowledge.Search(ctx, &knowledge.SearchRequest{Query: req.Query, MaxResults: 5})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) globalMemories(w http.ResponseWriter, r *http.Request) {
	entries, err := s.m.memories.Global(r.Context(), r.URL.Query().Get("query"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if entries == nil {
		entries = []*memory.Entry{}
	}
	writeJSON(w, 200, entries)
}
func (s *Server) deleteGlobalMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.m.memories.DeleteGlobal(r.Context(), r.PathValue("memory")); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) memoryTask(w http.ResponseWriter, r *http.Request) (memory.Service, int64) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "无效任务 ID")
		return nil, 0
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, 0
	}
	if task == nil {
		writeErr(w, 404, "任务不存在")
		return nil, 0
	}
	return s.m.memories.Task(id), id
}
func (s *Server) taskMemories(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.memoryTask(w, r)
	if svc == nil {
		return
	}
	entries, err := svc.ReadMemories(r.Context(), memory.UserKey{}, 1000)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if entries == nil {
		entries = []*memory.Entry{}
	}
	writeJSON(w, 200, entries)
}
func (s *Server) addTaskMemory(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.memoryTask(w, r)
	if svc == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<10)
	var req struct {
		Content string   `json:"content"`
		Topics  []string `json:"topics"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "无效的记忆内容")
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if err := svc.AddMemory(r.Context(), memory.UserKey{}, req.Content, req.Topics); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"saved": true})
}
func (s *Server) deleteTaskMemory(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.memoryTask(w, r)
	if svc == nil {
		return
	}
	if err := svc.DeleteMemory(r.Context(), memory.Key{MemoryID: r.PathValue("memory")}); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) distillTaskMemory(w http.ResponseWriter, r *http.Request) {
	svc, id := s.memoryTask(w, r)
	if svc == nil {
		return
	}
	mdl := s.m.memories.ModelForTask(id)
	if mdl == nil {
		writeErr(w, 400, "请为任务或默认配置提供可用的 LLM")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	entries, err := s.m.memories.Distill(ctx, id, mdl)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"saved": entries})
}

func (s *Server) retrievalStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.retrieval.Status())
}
