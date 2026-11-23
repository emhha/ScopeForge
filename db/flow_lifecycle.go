package db

import (
	"encoding/json"
	"fmt"
	"time"
)

const flowLifecycleSchema = `
CREATE TABLE IF NOT EXISTS flow_versions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 flow_id INTEGER NOT NULL REFERENCES flows(id),
 revision INTEGER NOT NULL,
 definition TEXT NOT NULL CHECK(json_valid(definition)),
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now()),
 UNIQUE(flow_id,revision)
);
CREATE TABLE IF NOT EXISTS flow_artifacts (
 id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL REFERENCES flow_runs(id),
 media_type TEXT NOT NULL,
 size INTEGER NOT NULL,
 sha256 TEXT NOT NULL,
 complete INTEGER NOT NULL,
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now())
);
CREATE INDEX IF NOT EXISTS idx_flow_artifacts_run ON flow_artifacts(run_id);
`

// v9 runs had a mandatory reusable Flow parent. Preserve events while rebuilding
// the parent/child tables, so inline runs can have no saved workflow at all.
const flowV10Migration = `
CREATE TEMP TABLE flow_events_backup AS SELECT * FROM flow_run_events;
DROP TABLE flow_run_events;
ALTER TABLE flow_runs RENAME TO flow_runs_v9;
CREATE TABLE flow_runs (
 id TEXT PRIMARY KEY, flow_id INTEGER REFERENCES flows(id),
 definition TEXT NOT NULL CHECK(json_valid(definition)),status TEXT NOT NULL,
 input TEXT NOT NULL CHECK(json_valid(input)),result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now()),completed_at TIMESTAMP,
 source TEXT NOT NULL DEFAULT 'manual',agent_key TEXT NOT NULL DEFAULT '',session_id TEXT NOT NULL DEFAULT '',task_id INTEGER NOT NULL DEFAULT 0,
 parent_run_id TEXT NOT NULL DEFAULT '',version_id INTEGER
);
INSERT INTO flow_runs(id,flow_id,definition,status,input,result,error,created_at,completed_at)
 SELECT id,flow_id,definition,status,input,result,error,created_at,completed_at FROM flow_runs_v9;
DROP TABLE flow_runs_v9;
CREATE TABLE flow_run_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES flow_runs(id) ON DELETE CASCADE,
 object TEXT NOT NULL,author TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',payload TEXT NOT NULL DEFAULT '{}',
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now())
);
INSERT INTO flow_run_events(id,run_id,object,author,error,created_at)
 SELECT id,run_id,object,author,error,created_at FROM flow_events_backup;
DROP TABLE flow_events_backup;
CREATE INDEX idx_flow_run_events ON flow_run_events(run_id,id);
CREATE INDEX idx_flow_runs_flow ON flow_runs(flow_id,created_at);
`

type FlowVersion struct {
	ID         int64           `json:"id"`
	FlowID     int64           `json:"flow_id"`
	Revision   int             `json:"revision"`
	Definition json.RawMessage `json:"definition"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (d *DB) FlowVersions(id int64) ([]FlowVersion, error) {
	rows, err := d.Query(`SELECT id,flow_id,revision,definition,created_at FROM flow_versions WHERE flow_id=?1 ORDER BY revision DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowVersion{}
	for rows.Next() {
		var v FlowVersion
		if err := rows.Scan(&v.ID, &v.FlowID, &v.Revision, &v.Definition, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (d *DB) GetFlowVersion(id int64) (FlowVersion, error) {
	var v FlowVersion
	err := d.QueryRow(`SELECT id,flow_id,revision,definition,created_at FROM flow_versions WHERE id=?1`, id).Scan(&v.ID, &v.FlowID, &v.Revision, &v.Definition, &v.CreatedAt)
	return v, err
}

// RegisterFlow atomically freezes a version and publishes a real custom tool.
func (d *DB) RegisterFlow(id int64, revision int, agents []string) (string, error) {
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var name string
	var raw json.RawMessage
	if err := tx.QueryRow(`SELECT name,definition FROM flows WHERE id=?1 AND revision=?2`, id, revision).Scan(&name, &raw); err != nil {
		return "", ErrFlowConflict
	}
	key := "flow_" + name
	var existingKind string
	var existingExec json.RawMessage
	err = tx.QueryRow(`SELECT kind,exec FROM tools WHERE key=?1`, key).Scan(&existingKind, &existingExec)
	if err == nil {
		var ref struct {
			FlowID int64 `json:"flow_id"`
		}
		_ = json.Unmarshal(existingExec, &ref)
		if existingKind != "workflow" || ref.FlowID != id {
			return "", fmt.Errorf("tool name %s is already in use", key)
		}
	}
	var def struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return "", err
	}
	var versionID int64
	if err := tx.QueryRow(`INSERT INTO flow_versions(flow_id,revision,definition) VALUES(?1,?2,?3) ON CONFLICT(flow_id,revision) DO UPDATE SET revision=excluded.revision RETURNING id`, id, revision, raw).Scan(&versionID); err != nil {
		return "", err
	}
	spec, _ := json.Marshal(map[string]any{"flow_id": id, "version_id": versionID})
	if _, err := tx.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled,kind,exec,deferred) VALUES(?1,false,?2,?3,?4,true,'workflow',?5,false)
 ON CONFLICT(key) DO UPDATE SET description=excluded.description,schema=excluded.schema,agents=excluded.agents,enabled=true,exec=excluded.exec`, key, def.Description, def.Schema, agents, spec); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE flows SET published=?2,agents=?3 WHERE id=?1`, id, raw, agents); err != nil {
		return "", err
	}
	return key, tx.Commit()
}

func (d *DB) AppendFlowPayload(run, object, author, message string, payload json.RawMessage) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := d.Exec(`INSERT INTO flow_run_events(run_id,object,author,error,payload) VALUES(?1,?2,?3,?4,?5)`, run, object, author, message, payload)
	return err
}

func (d *DB) SetFlowRunContext(id, source, key, parent string, task, version int64, session ...string) error {
	sid := ""
	if len(session) > 0 {
		sid = session[0]
	}
	_, err := d.Exec(`UPDATE flow_runs SET source=?2,agent_key=?3,parent_run_id=?4,task_id=?5,version_id=NULLIF(?6,0),session_id=?7 WHERE id=?1`, id, source, key, parent, task, version, sid)
	return err
}

// Publish the run identity and ownership atomically. A parent that is already
// terminal cannot acquire a new child after SSE has emitted its final frame.
func (d *DB) CreateWorkflowRun(run FlowRun) error {
	result, err := d.Exec(`INSERT INTO flow_runs(id,flow_id,definition,status,input,source,agent_key,task_id,session_id,parent_run_id,version_id)
 SELECT ?1,NULLIF(?2,0),?3,'running',?4,?5,?6,?7,?8,?9,NULLIF(?10,0)
 WHERE (?9='' OR EXISTS(SELECT 1 FROM flow_runs WHERE id=?9 AND status='running'))
 AND (?7=0 OR EXISTS(SELECT 1 FROM tasks WHERE id=?7 AND deleted_at IS NULL AND archived_at IS NULL))`, run.ID, run.FlowID, run.Definition, run.Input, run.Source, run.AgentKey, run.TaskID, run.SessionID, run.ParentRunID, run.VersionID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("parent workflow finished or task is no longer available")
	}
	return nil
}
func (d *DB) ListAllFlowRuns() ([]FlowRun, error) {
	rows, err := d.Query(`SELECT id FROM flow_runs ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []FlowRun{}
	for _, id := range ids {
		r, err := d.GetFlowRun(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}
