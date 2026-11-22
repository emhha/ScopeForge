package db

import (
	"encoding/json"
	"fmt"
)

func (d *DB) ImportFlowPublication(id int64, name string, revision int, raw json.RawMessage, agents []string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var def struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return err
	}
	if len(def.Schema) == 0 {
		def.Schema = json.RawMessage(`{"type":"object"}`)
	}
	var version int64
	if err := tx.QueryRow(`INSERT INTO flow_versions(flow_id,revision,definition) VALUES(?1,?2,?3) ON CONFLICT(flow_id,revision) DO UPDATE SET revision=excluded.revision RETURNING id`, id, revision, raw).Scan(&version); err != nil {
		return err
	}
	exec, _ := json.Marshal(map[string]any{"flow_id": id, "version_id": version})
	if _, err := tx.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled,kind,exec,deferred) VALUES(?1,false,?2,?3,?4,true,'workflow',?5,false) ON CONFLICT(key) DO NOTHING`, "flow_"+name, def.Description, def.Schema, agents, exec); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) UnpublishFlow(id int64, key string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tools SET enabled=false WHERE key=?1 AND kind='workflow' AND json_extract(exec,'$.flow_id')=?2`, key, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE flows SET published=NULL WHERE id=?1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) RollbackFlowPublication(id int64, v FlowVersion) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var key string
	if err := tx.QueryRow(`SELECT key FROM tools WHERE kind='workflow' AND json_extract(exec,'$.flow_id')=?1`, id).Scan(&key); err != nil {
		return fmt.Errorf("publish the workflow before rollback: %w", err)
	}
	var def struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(v.Definition, &def); err != nil {
		return err
	}
	exec, _ := json.Marshal(map[string]any{"flow_id": id, "version_id": v.ID})
	if _, err := tx.Exec(`UPDATE tools SET exec=?2,schema=?3,description=?4,enabled=true WHERE key=?1`, key, exec, def.Schema, def.Description); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE flows SET published=?2 WHERE id=?1`, id, v.Definition); err != nil {
		return err
	}
	return tx.Commit()
}
