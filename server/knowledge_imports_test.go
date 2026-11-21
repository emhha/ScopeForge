package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/emhha/scopeforge/db"
)

func TestLocalDirectoryImportRoutesPreserveClientRelativePaths(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := &Server{m: m}
	mux := http.NewServeMux()
	s.registerLearningRoutes(mux)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("kind", "directory")
	form.WriteField("name", "章节")
	form.WriteField("skipped", "3")
	form.WriteField("paths", `["章节/访问控制/guide.md"]`)
	f, _ := form.CreateFormFile("files", "guide.md")
	f.Write([]byte("# 权限\n检查对象归属。"))
	form.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/imports/preview", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var batch db.KnowledgeImport
	if err = json.Unmarshal(w.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Total != 1 || batch.Skipped != 3 || batch.Items[0].RelativePath != "章节/访问控制/guide.md" {
		t.Fatal(batch)
	}
	if docs, _ := m.pg.KnowledgeDocuments(); len(docs) != 0 {
		t.Fatal("preview implicitly indexed documents")
	}
	for _, path := range []string{"/api/retrieval/config", "/api/retrieval/status", "/api/retrieval/component"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/knowledge/imports/"+batch.ID+"/cancel", nil))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/knowledge/imports/"+batch.ID+"/start", nil))
	if w.Code != 400 {
		t.Fatal("cancelled preview started", w.Code)
	}
}

func TestImportRoutesRejectMissingPathManifest(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := &Server{m: m}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("kind", "directory")
	f, _ := form.CreateFormFile("files", "guide.md")
	f.Write([]byte("content"))
	form.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/imports/preview", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	s.previewKnowledgeImport(w, req)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}
