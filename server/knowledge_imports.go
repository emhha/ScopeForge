package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/learning"
)

func (s *Server) registerKnowledgeImportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/knowledge/imports", s.knowledgeImports)
	mux.HandleFunc("POST /api/knowledge/imports/preview", s.previewKnowledgeImport)
	mux.HandleFunc("GET /api/knowledge/imports/{batch}", s.knowledgeImport)
	mux.HandleFunc("POST /api/knowledge/imports/{batch}/start", s.startKnowledgeImport)
	mux.HandleFunc("POST /api/knowledge/imports/{batch}/cancel", s.cancelKnowledgeImport)
	mux.HandleFunc("POST /api/knowledge/imports/{batch}/retry", s.retryKnowledgeImport)
}
func (s *Server) knowledgeImports(w http.ResponseWriter, r *http.Request) {
	batches, err := s.m.pg.KnowledgeImports()
	if err != nil {
		writeErr(w, 500, "读取导入批次失败")
		return
	}
	writeJSON(w, 200, batches)
}
func (s *Server) knowledgeImport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("batch")
	state, err := s.m.pg.KnowledgeImportState(id)
	if err != nil {
		writeErr(w, 404, "导入批次不存在")
		return
	}
	items, err := s.m.pg.KnowledgeImportItems(id)
	if err != nil {
		writeErr(w, 500, "读取文件清单失败")
		return
	}
	writeJSON(w, 200, struct {
		Status string                   `json:"status"`
		Items  []db.KnowledgeImportItem `json:"items"`
	}{state, items})
}

func (s *Server) previewKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, learning.MaxImportBytes+(4<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "请选择目录或 ZIP 文件")
		return
	}
	// Upload spools live on the application's data volume, not the OS tmpfs.
	stage, err := os.MkdirTemp(s.m.knowledge.UploadDirectory(), "upload-")
	if err != nil {
		writeErr(w, 500, "无法暂存上传文件")
		return
	}
	defer os.RemoveAll(stage)
	var kind, name string
	var paths []string
	files := []learning.ImportFile{}
	archive := ""
	count := 0
	skipped := 0
	var total int64
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			writeErr(w, 400, "上传中断或超过批次大小限制")
			return
		}
		field := part.FormName()
		if part.FileName() == "" {
			data, e := io.ReadAll(io.LimitReader(part, (3<<20)+1))
			part.Close()
			if e != nil || len(data) > 3<<20 {
				writeErr(w, 400, "导入清单过大")
				return
			}
			switch field {
			case "kind":
				kind = string(data)
			case "name":
				name = string(data)
			case "skipped":
				skipped, _ = strconv.Atoi(string(data))
				if skipped < 0 || skipped > 20000 {
					writeErr(w, 400, "跳过数量无效")
					return
				}
			case "paths":
				if json.Unmarshal(data, &paths) != nil {
					writeErr(w, 400, "文件路径清单无效")
					return
				}
			default:
				writeErr(w, 400, "无效导入字段")
				return
			}
			continue
		}
		count++
		if count > 20000 {
			part.Close()
			writeErr(w, 400, "文件过多，请选择更小的章节目录")
			return
		}
		if kind != "directory" && kind != "zip" {
			part.Close()
			writeErr(w, 400, "请先指定目录或 ZIP 导入类型")
			return
		}
		if kind == "zip" && (field != "archive" || archive != "") {
			part.Close()
			writeErr(w, 400, "一次只能导入一个 ZIP")
			return
		}
		if kind == "directory" && (field != "files" || len(paths) < count) {
			part.Close()
			writeErr(w, 400, "文件与相对路径清单不匹配")
			return
		}
		limit := int64(learning.MaxDocumentBytes)
		if kind == "zip" {
			limit = learning.MaxImportZIPBytes
		}
		spool := filepath.Join(stage, strconv.Itoa(count))
		f, e := os.OpenFile(spool, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			part.Close()
			writeErr(w, 500, "无法保存上传文件")
			return
		}
		n, copyErr := io.Copy(f, io.LimitReader(part, limit+1))
		closeErr := f.Close()
		part.Close()
		total += n
		if copyErr != nil || closeErr != nil || n > limit || total > learning.MaxImportBytes {
			writeErr(w, 400, "文件超过单文件或批次大小限制")
			return
		}
		if kind == "zip" {
			archive = spool
		} else {
			files = append(files, learning.ImportFile{RelativePath: paths[count-1], Spool: spool})
		}
	}
	var batch db.KnowledgeImport
	if kind == "zip" && archive != "" {
		batch, err = s.m.knowledge.PreviewZIP(r.Context(), name, archive)
	} else if kind == "directory" && len(paths) == len(files) {
		batch, err = s.m.knowledge.PreviewDirectory(r.Context(), name, files, skipped)
	} else {
		err = errors.New("缺少文件或文件路径清单不完整")
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, batch)
}

func (s *Server) runImportAction(w http.ResponseWriter, r *http.Request, action func(string) error) {
	if err := action(r.PathValue("batch")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "导入批次不存在")
		} else {
			writeErr(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, 202, map[string]bool{"accepted": true})
}
func (s *Server) startKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	s.runImportAction(w, r, s.m.knowledge.StartImport)
}
func (s *Server) cancelKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	s.runImportAction(w, r, s.m.knowledge.CancelImport)
}
func (s *Server) retryKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	s.runImportAction(w, r, s.m.knowledge.RetryImport)
}
