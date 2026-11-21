package db

import "errors"

const knowledgeImportsSchema = `
CREATE TABLE IF NOT EXISTS knowledge_imports(
 id TEXT PRIMARY KEY,name TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'preview',
 skipped INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL DEFAULT (utc_now()));
CREATE TABLE IF NOT EXISTS knowledge_import_items(
 id TEXT PRIMARY KEY,import_id TEXT NOT NULL REFERENCES knowledge_imports(id) ON DELETE CASCADE,
 relative_path TEXT NOT NULL,name TEXT NOT NULL,sha256 TEXT NOT NULL,size INTEGER NOT NULL,
 staged_file TEXT NOT NULL,document_id TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'staged',error TEXT NOT NULL DEFAULT '',
 UNIQUE(import_id,relative_path));
CREATE INDEX IF NOT EXISTS knowledge_import_items_document ON knowledge_import_items(document_id);
CREATE INDEX IF NOT EXISTS knowledge_import_items_batch ON knowledge_import_items(import_id,status);`

type KnowledgeImport struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	Kind       string                `json:"kind"`
	Status     string                `json:"status"`
	Skipped    int                   `json:"skipped"`
	CreatedAt  string                `json:"created_at"`
	Total      int                   `json:"total"`
	Ready      int                   `json:"ready"`
	Failed     int                   `json:"failed"`
	Pending    int                   `json:"pending"`
	Duplicates int                   `json:"duplicates"`
	Cancelled  int                   `json:"cancelled"`
	Items      []KnowledgeImportItem `json:"items,omitempty"`
}
type KnowledgeImportItem struct {
	ID           string `json:"id"`
	ImportID     string `json:"import_id"`
	RelativePath string `json:"relative_path"`
	Name         string `json:"name"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	StagedFile   string `json:"-"`
	DocumentID   string `json:"document_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

func (d *DB) CreateKnowledgeImport(batch KnowledgeImport) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO knowledge_imports(id,name,kind,skipped) VALUES($1,$2,$3,$4)`, batch.ID, batch.Name, batch.Kind, batch.Skipped); err != nil {
		return err
	}
	for _, item := range batch.Items {
		if _, err = tx.Exec(`INSERT INTO knowledge_import_items(id,import_id,relative_path,name,sha256,size,staged_file,status,document_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, item.ID, batch.ID, item.RelativePath, item.Name, item.SHA256, item.Size, item.StagedFile, item.Status, item.DocumentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) KnowledgeImports() ([]KnowledgeImport, error) {
	rows, err := d.Query(`SELECT b.id,b.name,b.kind,b.status,b.skipped,b.created_at,count(i.id),
 coalesce(sum(i.status='ready'),0),coalesce(sum(i.status='failed'),0),coalesce(sum(i.status IN ('staged','queued','indexing')),0),
 coalesce(sum(i.status='duplicate'),0),coalesce(sum(i.status IN ('cancelled','deleted')),0)
 FROM knowledge_imports b LEFT JOIN knowledge_import_items i ON i.import_id=b.id GROUP BY b.id ORDER BY b.created_at DESC,b.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeImport{}
	for rows.Next() {
		var b KnowledgeImport
		if err = rows.Scan(&b.ID, &b.Name, &b.Kind, &b.Status, &b.Skipped, &b.CreatedAt, &b.Total, &b.Ready, &b.Failed, &b.Pending, &b.Duplicates, &b.Cancelled); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (d *DB) KnowledgeImportItems(id string) ([]KnowledgeImportItem, error) {
	rows, err := d.Query(`SELECT id,import_id,relative_path,name,sha256,size,staged_file,document_id,status,error FROM knowledge_import_items WHERE import_id=$1 ORDER BY relative_path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeImportItem{}
	for rows.Next() {
		var i KnowledgeImportItem
		if err = rows.Scan(&i.ID, &i.ImportID, &i.RelativePath, &i.Name, &i.SHA256, &i.Size, &i.StagedFile, &i.DocumentID, &i.Status, &i.Error); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (d *DB) KnowledgeImportState(id string) (string, error) {
	var status string
	err := d.QueryRow(`SELECT status FROM knowledge_imports WHERE id=$1`, id).Scan(&status)
	return status, err
}
func (d *DB) StartKnowledgeImport(id string) error {
	_, err := d.Exec(`UPDATE knowledge_imports SET status='queued' WHERE id=$1 AND status='preview'`, id)
	return err
}
func (d *DB) FinishKnowledgeImports() error {
	_, err := d.Exec(`UPDATE knowledge_imports SET status='done' WHERE status IN ('queued','importing') AND NOT EXISTS(SELECT 1 FROM knowledge_import_items i WHERE i.import_id=knowledge_imports.id AND i.status IN ('staged','queued','indexing'))`)
	return err
}
func (d *DB) FailKnowledgeImportItem(id, message string) error {
	_, err := d.Exec(`UPDATE knowledge_import_items SET status='failed',error=$2 WHERE id=$1 AND status='staged'`, id, message)
	return err
}
func (d *DB) AddKnowledgeImportDocument(item KnowledgeImportItem, doc KnowledgeDocument) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRow(`SELECT status FROM knowledge_imports WHERE id=$1`, item.ImportID).Scan(&status); err != nil {
		return err
	}
	if status != "queued" && status != "importing" {
		return errors.New("导入已停止")
	}
	if _, err = tx.Exec(`INSERT INTO knowledge_documents(id,name,sha256,size,import_id,relative_path) VALUES($1,$2,$3,$4,$5,$6)`, doc.ID, doc.Name, doc.SHA256, doc.Size, item.ImportID, item.RelativePath); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_import_items SET document_id=$2,status='queued',error='' WHERE id=$1`, item.ID, doc.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_imports SET status='importing' WHERE id=$1`, item.ImportID); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) CancelKnowledgeImport(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE knowledge_imports SET status='cancelled' WHERE id=$1 AND status<>'done'`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_documents SET status='cancelled',error='' WHERE import_id=$1 AND status IN ('queued','indexing')`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_import_items SET status='cancelled',error='' WHERE import_id=$1 AND status IN ('staged','queued','indexing')`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) RetryKnowledgeImport(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE knowledge_imports SET status='queued' WHERE id=$1 AND status='done'`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_documents SET status='queued',error='' WHERE import_id=$1 AND status='failed'`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_import_items SET status=CASE WHEN document_id='' THEN 'staged' ELSE 'queued' END,error='' WHERE import_id=$1 AND status='failed'`, id); err != nil {
		return err
	}
	return tx.Commit()
}
