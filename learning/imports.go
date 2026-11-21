package learning

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/emhha/scopeforge/db"
	"github.com/google/uuid"
)

const MaxImportFiles = 2000
const MaxImportBytes = 500 << 20
const MaxImportZIPBytes = 200 << 20

// ImportFile paths are server-created upload spools, never client filesystem
// paths. RelativePath is the browser's webkitRelativePath or the ZIP entry name.
type ImportFile struct {
	RelativePath string
	Spool        string
}

func supportedDocument(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".docx":
		return true
	}
	return false
}
func importPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || !utf8.ValidString(name) || len(name) > 1024 || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", errors.New("文件相对路径无效")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("文件路径不能包含上级目录")
		}
	}
	name = path.Clean(name)
	if name == "." || len(path.Base(name)) > 240 {
		return "", errors.New("文件名无效或过长")
	}
	return name, nil
}
func ignoredImportPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || part == "__MACOSX" {
			return true
		}
	}
	return false
}

type importCandidate struct {
	path string
	size uint64
	open func() (io.ReadCloser, error)
}

func (k *Knowledge) PreviewDirectory(ctx context.Context, name string, files []ImportFile, ignored ...int) (db.KnowledgeImport, error) {
	if len(files) > 20000 {
		return db.KnowledgeImport{}, errors.New("目录文件过多，请选择更小的章节目录")
	}
	candidates := []importCandidate{}
	skipped := 0
	if len(ignored) > 0 && ignored[0] >= 0 && ignored[0] <= 20000 {
		skipped = ignored[0]
	}
	for _, f := range files {
		rel, err := importPath(f.RelativePath)
		if err != nil {
			return db.KnowledgeImport{}, err
		}
		if !supportedDocument(rel) || ignoredImportPath(rel) {
			skipped++
			continue
		}
		stat, err := os.Lstat(f.Spool)
		if err != nil {
			return db.KnowledgeImport{}, err
		}
		if !stat.Mode().IsRegular() {
			return db.KnowledgeImport{}, errors.New("仅支持普通文档文件")
		}
		spool := f.Spool
		candidates = append(candidates, importCandidate{rel, uint64(stat.Size()), func() (io.ReadCloser, error) { return os.Open(spool) }})
	}
	return k.previewImport(ctx, name, "directory", candidates, skipped)
}
func (k *Knowledge) PreviewZIP(ctx context.Context, name, spool string) (db.KnowledgeImport, error) {
	z, err := zip.OpenReader(spool)
	if err != nil {
		return db.KnowledgeImport{}, errors.New("无法读取 ZIP，请检查压缩包格式")
	}
	defer z.Close()
	if len(z.File) > 20000 {
		return db.KnowledgeImport{}, errors.New("ZIP 文件条目过多，请选择更小的章节压缩包")
	}
	candidates := []importCandidate{}
	skipped := 0
	for _, f := range z.File {
		rel, err := importPath(f.Name)
		if err != nil {
			return db.KnowledgeImport{}, err
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return db.KnowledgeImport{}, errors.New("ZIP 不能包含符号链接或特殊文件")
		}
		if !supportedDocument(rel) || ignoredImportPath(rel) {
			skipped++
			continue
		}
		entry := f
		candidates = append(candidates, importCandidate{rel, f.UncompressedSize64, entry.Open})
	}
	return k.previewImport(ctx, name, "zip", candidates, skipped)
}

func (k *Knowledge) previewImport(ctx context.Context, name, kind string, candidates []importCandidate, skipped int) (db.KnowledgeImport, error) {
	batch := db.KnowledgeImport{ID: uuid.NewString(), Name: strings.TrimSpace(name), Kind: kind, Status: "preview", Skipped: skipped, Items: []db.KnowledgeImportItem{}}
	if batch.Name == "" {
		batch.Name = "本地文档"
	}
	if len([]rune(batch.Name)) > 100 {
		return batch, errors.New("导入名称不超过 100 个字符")
	}
	if len(candidates) == 0 {
		return batch, errors.New("未找到 Markdown、TXT 或 DOCX 文档")
	}
	if len(candidates) > MaxImportFiles {
		return batch, fmt.Errorf("每批最多 %d 个文档", MaxImportFiles)
	}
	var total uint64
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.size > MaxDocumentBytes {
			return batch, fmt.Errorf("%s 超过单文件 20 MiB 限制", c.path)
		}
		total += c.size
		if total > MaxImportBytes {
			return batch, errors.New("文档解压后总大小超过 500 MiB")
		}
		if seen[c.path] {
			return batch, errors.New("导入包含重复相对路径")
		}
		seen[c.path] = true
	}
	root := filepath.Join(k.dir, "imports")
	if err := os.MkdirAll(root, 0700); err != nil {
		return batch, err
	}
	stage := filepath.Join(root, batch.ID)
	if err := os.Mkdir(stage, 0700); err != nil {
		return batch, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(stage)
		}
	}()
	existing, err := k.pg.KnowledgeDocuments()
	if err != nil {
		return batch, err
	}
	duplicates := map[string]string{}
	for _, d := range existing {
		if d.Status == "ready" {
			duplicates[d.RelativePath+"\x00"+d.SHA256] = d.ID
		}
	}
	sort.Slice(candidates, func(a, b int) bool { return candidates[a].path < candidates[b].path })
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		item := db.KnowledgeImportItem{ID: uuid.NewString(), ImportID: batch.ID, RelativePath: c.path, Name: path.Base(c.path), Status: "staged"}
		item.StagedFile = item.ID + path.Ext(item.Name)
		r, err := c.open()
		if err != nil {
			return batch, err
		}
		f, err := os.OpenFile(filepath.Join(stage, item.StagedFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			r.Close()
			return batch, err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxDocumentBytes+1))
		readClose, writeClose := r.Close(), f.Close()
		if err = errors.Join(copyErr, readClose, writeClose); err != nil {
			return batch, fmt.Errorf("读取 %s 失败: %w", c.path, err)
		}
		if n == 0 || n > MaxDocumentBytes || uint64(n) != c.size {
			return batch, fmt.Errorf("%s 为空、损坏或大小不符", c.path)
		}
		item.Size = n
		item.SHA256 = hex.EncodeToString(h.Sum(nil))
		if docID := duplicates[item.RelativePath+"\x00"+item.SHA256]; docID != "" {
			item.Status = "duplicate"
			item.DocumentID = docID
			os.Remove(filepath.Join(stage, item.StagedFile))
		}
		batch.Items = append(batch.Items, item)
	}
	if err = k.pg.CreateKnowledgeImport(batch); err != nil {
		return batch, err
	}
	committed = true
	batch.Total = len(batch.Items)
	for _, i := range batch.Items {
		if i.Status == "duplicate" {
			batch.Duplicates++
		} else {
			batch.Pending++
		}
	}
	return batch, nil
}

func (k *Knowledge) StartImport(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	state, err := k.pg.KnowledgeImportState(id)
	if err != nil {
		return err
	}
	if state != "preview" {
		return errors.New("该批次已开始或已停止")
	}
	if err = k.pg.StartKnowledgeImport(id); err != nil {
		return err
	}
	k.signal()
	return nil
}
func (k *Knowledge) CancelImport(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	state, err := k.pg.KnowledgeImportState(id)
	if err != nil {
		return err
	}
	if state == "done" {
		return errors.New("该批次已完成")
	}
	if err = k.pg.CancelKnowledgeImport(id); err != nil {
		return err
	}
	if k.activeDoc != "" {
		var batch string
		_ = k.pg.QueryRow(`SELECT import_id FROM knowledge_documents WHERE id=?1`, k.activeDoc).Scan(&batch)
		if batch == id && k.activeCancel != nil {
			k.activeCancel()
		}
	}
	return os.RemoveAll(filepath.Join(k.dir, "imports", id))
}
func (k *Knowledge) RetryImport(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	state, err := k.pg.KnowledgeImportState(id)
	if err != nil {
		return err
	}
	if state != "done" {
		return errors.New("仅已完成批次可以重试失败文档")
	}
	if err = k.pg.RetryKnowledgeImport(id); err != nil {
		return err
	}
	k.signal()
	return nil
}

func (k *Knowledge) importNext() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	var item db.KnowledgeImportItem
	err := k.pg.QueryRow(`SELECT i.id,i.import_id,i.relative_path,i.name,i.sha256,i.size,i.staged_file FROM knowledge_import_items i JOIN knowledge_imports b ON b.id=i.import_id WHERE b.status IN ('queued','importing') AND i.status='staged' ORDER BY b.created_at,i.relative_path LIMIT 1`).Scan(&item.ID, &item.ImportID, &item.RelativePath, &item.Name, &item.SHA256, &item.Size, &item.StagedFile)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		return false
	}
	doc := db.KnowledgeDocument{ID: uuid.NewString(), Name: item.Name, SHA256: item.SHA256, Size: item.Size, ImportID: item.ImportID, RelativePath: item.RelativePath}
	err = func() error {
		r, err := os.Open(filepath.Join(k.dir, "imports", item.ImportID, item.StagedFile))
		if err != nil {
			return err
		}
		defer r.Close()
		f, err := os.OpenFile(k.path(doc), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(r, MaxDocumentBytes+1))
		closeErr := f.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if n != item.Size {
			return errors.New("原文件大小不符")
		}
		return k.pg.AddKnowledgeImportDocument(item, doc)
	}()
	if err != nil {
		os.Remove(k.path(doc))
		_ = k.pg.FailKnowledgeImportItem(item.ID, "原文件无法读取，请检查文件后重试或重新导入")
	} else {
		_ = os.Remove(filepath.Join(k.dir, "imports", item.ImportID, item.StagedFile))
	}
	return true
}
