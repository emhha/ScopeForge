package learning

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/db"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
)

func importSpool(t *testing.T, name, text string) ImportFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spool")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return ImportFile{RelativePath: name, Spool: p}
}
func waitImport(t *testing.T, pg *db.DB, id string) db.KnowledgeImport {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		batches, err := pg.KnowledgeImports()
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range batches {
			if b.ID == id && b.Status == "done" {
				return b
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("batch did not finish")
	return db.KnowledgeImport{}
}

func TestDirectoryPreviewRestartImportFailuresAndDeduplication(t *testing.T) {
	pg, dir := testDB(t)
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	files := []ImportFile{importSpool(t, "chapter/access/guide.md", "# 权限验证\n先检查对象归属和会话身份。"), importSpool(t, "chapter/cache.txt", "排除缓存干扰后再做验证。"), importSpool(t, "chapter/broken.docx", "not a valid docx"), {RelativePath: "chapter/image.png"}}
	b, err := k.PreviewDirectory(t.Context(), "章节", files)
	if err != nil {
		t.Fatal(err)
	}
	if b.Total != 3 || b.Skipped != 1 {
		t.Fatal(b)
	}
	if docs, _ := pg.KnowledgeDocuments(); len(docs) != 0 {
		t.Fatal("preview indexed documents before confirmation")
	}
	k.Close()
	k, err = OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err = k.StartImport(b.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitImport(t, pg, b.ID)
	if finished.Ready != 2 || finished.Failed != 1 {
		t.Fatal(finished)
	}
	result, err := k.Search(t.Context(), &knowledge.SearchRequest{Query: "对象归属"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("keyword search %+v %v", result, err)
	}
	if result.Documents[0].Document.Metadata["relative_path"] != "chapter/access/guide.md" {
		t.Fatal("relative path was not retained")
	}
	// A parse failure retains its original and can be repaired/retried without
	// reuploading or reindexing the already successful documents.
	docs, _ := pg.KnowledgeDocuments()
	for _, d := range docs {
		if d.Status == "failed" {
			if err = writeDOCX(k.path(d), "修复后的文档内容"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = k.RetryImport(b.ID); err != nil {
		t.Fatal(err)
	}
	finished = waitImport(t, pg, b.ID)
	if finished.Ready != 3 || finished.Failed != 0 {
		t.Fatal(finished)
	}
	dup, err := k.PreviewDirectory(t.Context(), "再次导入", files[:2])
	if err != nil {
		t.Fatal(err)
	}
	if dup.Duplicates != 2 {
		t.Fatal("same path/content was not skipped", dup)
	}
	if err = k.StartImport(dup.ID); err != nil {
		t.Fatal(err)
	}
	finished = waitImport(t, pg, dup.ID)
	if finished.Duplicates != 2 {
		t.Fatal(finished)
	}
}

func writeDOCX(file, text string) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	w, err := z.Create("word/document.xml")
	if err == nil {
		_, err = w.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`))
	}
	if err == nil {
		var rel io.Writer
		rel, err = z.Create("word/_rels/document.xml.rels")
		if err == nil {
			_, err = rel.Write([]byte(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`))
		}
	}
	closeZIP, closeFile := z.Close(), f.Close()
	if err != nil {
		return err
	}
	if closeZIP != nil {
		return closeZIP
	}
	return closeFile
}
func zipSpool(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chapter.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, text := range entries {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestZIPPreviewCancelAndUnsafeInputs(t *testing.T) {
	pg, dir := testDB(t)
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	b, err := k.PreviewZIP(t.Context(), "zip chapter", zipSpool(t, map[string]string{"chapter/guide.md": "先检查权限边界。", "chapter/image.png": "image", "__MACOSX/guide.md": "metadata"}))
	if err != nil {
		t.Fatal(err)
	}
	if b.Total != 1 || b.Skipped != 2 {
		t.Fatal(b)
	}
	if err = k.CancelImport(b.ID); err != nil {
		t.Fatal(err)
	}
	if err = k.StartImport(b.ID); err == nil {
		t.Fatal("cancelled batch restarted")
	}
	if docs, _ := pg.KnowledgeDocuments(); len(docs) != 0 {
		t.Fatal("cancelled preview created documents")
	}
	if _, err = os.Stat(filepath.Join(k.dir, "imports", b.ID)); !os.IsNotExist(err) {
		t.Fatal("cancelled staging directory retained")
	}
	for _, name := range []string{"../escape.md", "/absolute.md", "a/../../escape.md", "C:\\escape.md"} {
		if _, err = k.PreviewZIP(t.Context(), "bad", zipSpool(t, map[string]string{name: "unsafe"})); err == nil {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	if _, err = k.PreviewZIP(t.Context(), "oversize", zipSpool(t, map[string]string{"large.md": strings.Repeat("x", MaxDocumentBytes+1)})); err == nil {
		t.Fatal("accepted oversized expanded document")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = k.PreviewDirectory(ctx, "cancelled", []ImportFile{importSpool(t, "a.md", "content")}); err == nil {
		t.Fatal("ignored request cancellation")
	}
}

func TestSharedRetrievalOutlivesKnowledgeAndMigratesConfiguration(t *testing.T) {
	pg, dir := testDB(t)
	if err := pg.SetSetting("knowledge_embedding", `{"mode":"none","model":"retained","base_url":"http://127.0.0.1:1/v1","dimensions":3}`); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRetrieval(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	k, err := OpenKnowledgeWithRetrieval(dir, pg, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Model != "retained" {
		t.Fatal("legacy configuration lost")
	}
	if err = k.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.Configure(EmbeddingConfig{Mode: "none"}); err != nil {
		t.Fatal("knowledge closure stopped shared retrieval", err)
	}
	if _, err = r.cache.Exec(`SELECT 1`); err != nil {
		t.Fatal("knowledge closure closed shared cache", err)
	}
}

func TestCancellingQueuedBatchKeepsCompletedDocuments(t *testing.T) {
	pg, dir := testDB(t)
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	// Settle the execution worker so cancellation is checked at a deterministic
	// boundary between copying an original and starting its parser.
	k.cancel()
	k.wg.Wait()
	b, err := k.PreviewDirectory(t.Context(), "cancel boundary", []ImportFile{importSpool(t, "chapter/a.md", "完成的文档"), importSpool(t, "chapter/b.md", "尚未解析的文档")})
	if err != nil {
		t.Fatal(err)
	}
	if err = k.StartImport(b.ID); err != nil {
		t.Fatal(err)
	}
	if !k.importNext() {
		t.Fatal("first original not staged")
	}
	docs, _ := pg.KnowledgeDocuments()
	if len(docs) != 1 {
		t.Fatal(docs)
	}
	completed := docs[0]
	if err = pg.SetKnowledgeDocumentStatus(completed.ID, "ready", "", 1); err != nil {
		t.Fatal(err)
	}
	if !k.importNext() {
		t.Fatal("second original not staged")
	}
	if err = k.CancelImport(b.ID); err != nil {
		t.Fatal(err)
	}
	docs, _ = pg.KnowledgeDocuments()
	for _, d := range docs {
		if d.ID == completed.ID && d.Status != "ready" {
			t.Fatal("cancel removed successful document")
		}
		if d.ID != completed.ID && d.Status != "cancelled" {
			t.Fatal("pending document was not cancelled", d)
		}
	}
	if k.indexNext() {
		t.Fatal("cancelled document was parsed again")
	}
	if _, err = os.Stat(k.path(completed)); err != nil {
		t.Fatal("completed original removed", err)
	}
}
