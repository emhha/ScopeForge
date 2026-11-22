package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const flowSchema = `
CREATE TABLE IF NOT EXISTS flows (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL UNIQUE,
 definition TEXT NOT NULL CHECK(json_valid(definition)),
 published TEXT CHECK(published IS NULL OR json_valid(published)),
 revision INTEGER NOT NULL DEFAULT 1,
 agents TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(agents)),
 updated_at TIMESTAMP NOT NULL DEFAULT (utc_now())
);
CREATE TABLE IF NOT EXISTS flow_runs (
 id TEXT PRIMARY KEY,
 flow_id INTEGER REFERENCES flows(id) ON DELETE RESTRICT,
 definition TEXT NOT NULL CHECK(json_valid(definition)),
 status TEXT NOT NULL,
 input TEXT NOT NULL CHECK(json_valid(input)),
 result TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now()),
 completed_at TIMESTAMP,
 source TEXT NOT NULL DEFAULT 'manual',
 agent_key TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '',
 task_id INTEGER NOT NULL DEFAULT 0,
 parent_run_id TEXT NOT NULL DEFAULT '',
 version_id INTEGER
);
CREATE TABLE IF NOT EXISTS flow_run_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 run_id TEXT NOT NULL REFERENCES flow_runs(id) ON DELETE CASCADE,
 object TEXT NOT NULL,
 author TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '',
 payload TEXT NOT NULL DEFAULT '{}',
 created_at TIMESTAMP NOT NULL DEFAULT (utc_now())
);
CREATE INDEX IF NOT EXISTS idx_flow_run_events ON flow_run_events(run_id,id);
CREATE INDEX IF NOT EXISTS idx_flow_runs_flow ON flow_runs(flow_id,created_at);
`

type Flow struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	Published  json.RawMessage `json:"published,omitempty"`
	Revision   int             `json:"revision"`
	Agents     []string        `json:"agents"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

var ErrFlowConflict = errors.New("flow was changed; reload before saving")

func (d *DB) ListFlows() ([]Flow, error) {
	rows, err := d.Query(`SELECT id,name,definition,COALESCE(published,'null'),revision,COALESCE((SELECT agents FROM tools WHERE kind='workflow' AND json_extract(exec,'$.flow_id')=flows.id LIMIT 1),agents),updated_at FROM flows ORDER BY updated_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Flow{}
	for rows.Next() {
		var f Flow
		var agents string
		if err := rows.Scan(&f.ID, &f.Name, &f.Definition, &f.Published, &f.Revision, &agents, &f.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(agents), &f.Agents); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (d *DB) GetFlow(id int64) (*Flow, error) {
	var f Flow
	var agents string
	err := d.QueryRow(`SELECT id,name,definition,COALESCE(published,'null'),revision,COALESCE((SELECT agents FROM tools WHERE kind='workflow' AND json_extract(exec,'$.flow_id')=flows.id LIMIT 1),agents),updated_at FROM flows WHERE id=?1`, id).Scan(&f.ID, &f.Name, &f.Definition, &f.Published, &f.Revision, &agents, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(agents), &f.Agents); err != nil {
		return nil, err
	}
	return &f, nil
}
func (d *DB) SaveFlow(id int64, name string, definition json.RawMessage, revision int) (int64, error) {
	if id == 0 {
		var created int64
		err := d.QueryRow(`INSERT INTO flows(name,definition) VALUES(?1,?2) RETURNING id`, name, definition).Scan(&created)
		return created, err
	}
	result, err := d.Exec(`UPDATE flows SET name=?2,definition=?3,revision=revision+1,updated_at=utc_now() WHERE id=?1 AND revision=?4`, id, name, definition, revision)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrFlowConflict
	}
	return id, nil
}
func (d *DB) PublishFlow(id int64, revision int, definition json.RawMessage, agents []string) error {
	result, err := d.Exec(`UPDATE flows SET published=?3,agents=?4,updated_at=utc_now() WHERE id=?1 AND revision=?2`, id, revision, definition, agents)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrFlowConflict
	}
	return nil
}

type FlowRun struct {
	SessionID   string          `json:"session_id"`
	Source      string          `json:"source"`
	AgentKey    string          `json:"agent_key"`
	TaskID      int64           `json:"task_id"`
	ParentRunID string          `json:"parent_run_id"`
	VersionID   int64           `json:"version_id"`
	ID          string          `json:"id"`
	FlowID      int64           `json:"flow_id"`
	Definition  json.RawMessage `json:"definition"`
	Status      string          `json:"status"`
	Input       json.RawMessage `json:"input"`
	Result      string          `json:"result"`
	Error       string          `json:"error"`
	CreatedAt   time.Time       `json:"created_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}
type FlowRunEvent struct {
	Payload   json.RawMessage `json:"payload"`
	ID        int64           `json:"id"`
	RunID     string          `json:"run_id"`
	Object    string          `json:"object"`
	Author    string          `json:"author"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

func (d *DB) CreateFlowRun(id string, flowID int64, definition, input json.RawMessage) error {
	_, err := d.Exec(`INSERT INTO flow_runs(id,flow_id,definition,status,input) VALUES(?1,NULLIF(?2,0),?3,'running',?4)`, id, flowID, definition, input)
	return err
}
func (d *DB) GetFlowRun(id string) (*FlowRun, error) {
	var r FlowRun
	err := d.QueryRow(`SELECT id,COALESCE(flow_id,0),definition,status,input,result,error,created_at,completed_at,source,agent_key,task_id,parent_run_id,COALESCE(version_id,0),session_id FROM flow_runs WHERE id=?1`, id).Scan(&r.ID, &r.FlowID, &r.Definition, &r.Status, &r.Input, &r.Result, &r.Error, &r.CreatedAt, &r.CompletedAt, &r.Source, &r.AgentKey, &r.TaskID, &r.ParentRunID, &r.VersionID, &r.SessionID)
	return &r, err
}
func (d *DB) FinishFlowRun(id, status, result, message string) error {
	if status != "completed" && status != "failed" && status != "cancelled" && status != "interrupted" {
		return fmt.Errorf("invalid flow run status")
	}
	_, err := d.Exec(`UPDATE flow_runs SET status=?2,result=?3,error=?4,completed_at=utc_now() WHERE id=?1 AND status='running'`, id, status, result, message)
	return err
}
func (d *DB) AppendFlowEvent(run, object, author, message string) error {
	_, err := d.Exec(`INSERT INTO flow_run_events(run_id,object,author,error) VALUES(?1,?2,?3,?4)`, run, object, author, message)
	return err
}
func (d *DB) FlowEvents(run string, since int64) ([]FlowRunEvent, error) {
	rows, err := d.Query(`SELECT id,run_id,object,author,error,created_at,payload FROM flow_run_events WHERE run_id=?1 AND id>?2 ORDER BY id LIMIT 500`, run, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []FlowRunEvent{}
	for rows.Next() {
		var e FlowRunEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.Object, &e.Author, &e.Error, &e.CreatedAt, &e.Payload); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

type FlowRunSummary struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func (d *DB) ListFlowRuns(flowID int64) ([]FlowRunSummary, error) {
	rows, err := d.Query(`SELECT id,status,COALESCE(json_extract(definition,'$.version'),1),created_at FROM flow_runs WHERE flow_id=?1 ORDER BY created_at DESC LIMIT 20`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []FlowRunSummary{}
	for rows.Next() {
		var item FlowRunSummary
		if err := rows.Scan(&item.ID, &item.Status, &item.Version, &item.CreatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, item)
	}
	return runs, rows.Err()
}
