package learning

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/emhha/scopeforge/db"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
)

// Keep individual parser failures inside the import job. Actual text reading
// and chunking still use the native tRPC reader/source implementations.
func (k *Knowledge) readDocument(ctx context.Context, d db.KnowledgeDocument) (docs []*document.Document, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			docs = nil
			err = fmt.Errorf("document reader failed: %v", recovered)
		}
	}()
	if strings.EqualFold(filepath.Ext(d.Name), ".docx") {
		z, e := zip.OpenReader(k.path(d))
		if e != nil {
			return nil, e
		}
		var total uint64
		documentFound, relationsFound := false, false
		for _, f := range z.File {
			total += f.UncompressedSize64
			if total > 100<<20 {
				z.Close()
				return nil, errors.New("DOCX expanded content exceeds limit")
			}
			if f.Name == "word/document.xml" {
				documentFound = true
			}
			if f.Name == "word/_rels/document.xml.rels" {
				relationsFound = true
			}
		}
		z.Close()
		if !documentFound || !relationsFound {
			return nil, errors.New("DOCX structure is incomplete")
		}
	}
	return k.source(d).ReadDocuments(ctx)
}
