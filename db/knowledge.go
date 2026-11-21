package db

// KnowledgeDocument tracks an uploaded original, not native vector-store rows.
type KnowledgeDocument struct {
	ImportID     string `json:"import_id"`
	RelativePath string `json:"relative_path"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	Chunks       int    `json:"chunks"`
	CreatedAt    string `json:"created_at"`
}

func (d *DB) KnowledgeDocuments() ([]KnowledgeDocument, error) {
	rows, err := d.Query(`SELECT id,name,sha256,size,status,error,chunks,created_at,import_id,relative_path FROM knowledge_documents ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeDocument{}
	for rows.Next() {
		var v KnowledgeDocument
		if err := rows.Scan(&v.ID, &v.Name, &v.SHA256, &v.Size, &v.Status, &v.Error, &v.Chunks, &v.CreatedAt, &v.ImportID, &v.RelativePath); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (d *DB) AddKnowledgeDocument(v KnowledgeDocument) error {
	_, err := d.Exec(`INSERT INTO knowledge_documents(id,name,sha256,size) VALUES($1,$2,$3,$4)`, v.ID, v.Name, v.SHA256, v.Size)
	return err
}
func (d *DB) SetKnowledgeDocumentStatus(id, status, message string, chunks int) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE knowledge_documents SET status=$2,error=$3,chunks=$4 WHERE id=$1`, id, status, message, chunks); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE knowledge_import_items SET status=$2,error=$3 WHERE document_id=$1 AND status NOT IN ('cancelled','duplicate')`, id, status, message); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) DeleteKnowledgeDocument(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE knowledge_import_items SET status='deleted' WHERE document_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM knowledge_documents WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
