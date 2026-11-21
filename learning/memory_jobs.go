package learning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

const scopeSchema = `CREATE TABLE IF NOT EXISTS scope_state(
 app_name TEXT NOT NULL,user_id TEXT NOT NULL,revision INTEGER NOT NULL DEFAULT 1,
 synced_revision INTEGER NOT NULL DEFAULT -1,generation TEXT NOT NULL DEFAULT '',
 distilled_revision INTEGER NOT NULL DEFAULT 0,changed_at INTEGER NOT NULL DEFAULT (unixepoch()),
 due_at INTEGER NOT NULL DEFAULT (unixepoch()+60),attempts INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',PRIMARY KEY(app_name,user_id));
CREATE TABLE IF NOT EXISTS blocked_tasks(user_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS memory_flags(id INTEGER PRIMARY KEY CHECK(id=1),task_enabled INTEGER NOT NULL DEFAULT 1);
INSERT OR IGNORE INTO memory_flags(id) VALUES(1);
INSERT OR IGNORE INTO scope_state(app_name,user_id) SELECT DISTINCT app_name,user_id FROM memories;
CREATE TRIGGER IF NOT EXISTS memory_scope_insert AFTER INSERT ON memories BEGIN
 INSERT INTO scope_state(app_name,user_id) VALUES(NEW.app_name,NEW.user_id)
 ON CONFLICT(app_name,user_id) DO UPDATE SET revision=revision+1,changed_at=unixepoch(),due_at=unixepoch()+60,attempts=0,retry_at=0,error=''; END;
CREATE TRIGGER IF NOT EXISTS memory_scope_update AFTER UPDATE ON memories BEGIN
 INSERT INTO scope_state(app_name,user_id) VALUES(NEW.app_name,NEW.user_id)
 ON CONFLICT(app_name,user_id) DO UPDATE SET revision=revision+1,changed_at=unixepoch(),due_at=unixepoch()+60,attempts=0,retry_at=0,error=''; END;
CREATE TRIGGER IF NOT EXISTS memory_scope_delete AFTER DELETE ON memories BEGIN
 INSERT INTO scope_state(app_name,user_id) VALUES(OLD.app_name,OLD.user_id)
 ON CONFLICT(app_name,user_id) DO UPDATE SET revision=revision+1,changed_at=unixepoch(),due_at=unixepoch()+60,attempts=0,retry_at=0,error=''; END;
CREATE TRIGGER IF NOT EXISTS memory_task_insert_guard BEFORE INSERT ON memories
 WHEN NEW.app_name='scopeforge-task' AND EXISTS(SELECT 1 FROM blocked_tasks WHERE user_id=NEW.user_id)
 BEGIN SELECT RAISE(ABORT,'task memory is deleted'); END;
CREATE TRIGGER IF NOT EXISTS memory_task_update_guard BEFORE UPDATE ON memories
 WHEN NEW.app_name='scopeforge-task' AND EXISTS(SELECT 1 FROM blocked_tasks WHERE user_id=NEW.user_id)
 BEGIN SELECT RAISE(ABORT,'task memory is deleted'); END;
CREATE TRIGGER IF NOT EXISTS memory_task_insert_enabled_guard BEFORE INSERT ON memories
 WHEN NEW.app_name='scopeforge-task' AND (SELECT task_enabled FROM memory_flags WHERE id=1)=0
 BEGIN SELECT RAISE(ABORT,'task memory is disabled'); END;
CREATE TRIGGER IF NOT EXISTS memory_task_update_enabled_guard BEFORE UPDATE ON memories
 WHEN NEW.app_name='scopeforge-task' AND (SELECT task_enabled FROM memory_flags WHERE id=1)=0
 BEGIN SELECT RAISE(ABORT,'task memory is disabled'); END;`

type JobStatus struct {
	Pending  int `json:"pending"`
	Retrying int `json:"retrying"`
}

func (m *Memories) JobStatus() JobStatus {
	var s JobStatus
	_ = m.conn.QueryRow(`SELECT count(*),coalesce(sum(CASE WHEN attempts>0 THEN 1 ELSE 0 END),0) FROM scope_state WHERE app_name='scopeforge-task' AND revision>distilled_revision AND user_id NOT IN (SELECT user_id FROM blocked_tasks)`).Scan(&s.Pending, &s.Retrying)
	var pending, retrying int
	_ = m.conn.QueryRow(`SELECT count(*),coalesce(sum(CASE WHEN attempts>0 THEN 1 ELSE 0 END),0) FROM checkpoints WHERE revision<>processed`).Scan(&pending, &retrying)
	s.Pending += pending
	s.Retrying += retrying
	return s
}

func (m *Memories) SetModelProvider(fn func() model.Model) {
	m.providerMu.Lock()
	m.provider = fn
	m.providerMu.Unlock()
}
func (m *Memories) SetTaskModelProvider(fn func(int64) model.Model) {
	m.providerMu.Lock()
	m.taskProvider = fn
	m.providerMu.Unlock()
}
func (m *Memories) ModelForTask(id int64) model.Model {
	m.providerMu.RLock()
	fn := m.taskProvider
	m.providerMu.RUnlock()
	if fn != nil {
		if mdl := fn(id); mdl != nil {
			return mdl
		}
	}
	return m.model()
}
func (m *Memories) model() model.Model {
	m.providerMu.RLock()
	fn := m.provider
	m.providerMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}
func (m *Memories) SetSemanticIndex(s *SemanticIndex) {
	m.providerMu.Lock()
	m.semantic = s
	m.providerMu.Unlock()
}
func (m *Memories) semanticIndex() *SemanticIndex {
	m.providerMu.RLock()
	defer m.providerMu.RUnlock()
	return m.semantic
}

// QueueDistill is non-blocking with respect to models. Triggered at committed
// worker/task boundaries; no memory changes means no extraction request.
func (m *Memories) QueueDistill(id int64) {
	if id <= 0 {
		return
	}
	_, _ = m.conn.Exec(`UPDATE scope_state SET due_at=unixepoch(),retry_at=0 WHERE app_name=? AND user_id=?`, taskKey(id).AppName, taskKey(id).UserID)
}
func (m *Memories) work() {
	defer m.wg.Done()
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		if s := m.semanticIndex(); s != nil && s.Status().Ready {
			_ = m.syncOneScope(m.ctx, s)
		}
		_ = m.checkpointNext(m.ctx)
		settings := m.Settings()
		if settings.TaskEnabled && settings.GlobalEnabled && settings.AutoDistill {
			_ = m.distillNext(m.ctx)
		}
	}
}
func (m *Memories) syncOneScope(ctx context.Context, s *SemanticIndex) error {
	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	_, generation := s.remote()
	var key memory.UserKey
	var revision int64
	settings := m.Settings()
	err := m.conn.QueryRowContext(ctx, `SELECT app_name,user_id,revision FROM scope_state WHERE (synced_revision<>revision OR generation<>?) AND ((app_name='scopeforge-task' AND ?) OR (app_name='scopeforge-global' AND ?)) ORDER BY changed_at LIMIT 1`, generation, settings.TaskEnabled, settings.GlobalEnabled).Scan(&key.AppName, &key.UserID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := m.store.ReadMemories(ctx, key, 1000)
	if err != nil {
		return err
	}
	// A task may have been deleted without application cleanup before restart.
	if key.AppName == "scopeforge-task" {
		id, _ := strconv.ParseInt(strings.TrimPrefix(key.UserID, "task:"), 10, 64)
		task, err := m.pg.GetTask(id)
		if err != nil {
			return err
		}
		if task == nil {
			entries = nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.Sync(ctx, key, entries); err != nil {
		return err
	}
	if _, current := s.remote(); current != generation {
		return nil
	}
	_, err = m.conn.ExecContext(ctx, `UPDATE scope_state SET synced_revision=?,generation=? WHERE app_name=? AND user_id=? AND revision=?`, revision, generation, key.AppName, key.UserID, revision)
	return err
}
func (m *Memories) distillNext(ctx context.Context) error {
	var user string
	var revision int64
	var attempts int
	err := m.conn.QueryRowContext(ctx, `SELECT user_id,revision,attempts FROM scope_state WHERE app_name='scopeforge-task' AND revision>distilled_revision AND due_at<=unixepoch() AND retry_at<=unixepoch() AND (?=0 OR NOT EXISTS(SELECT 1 FROM checkpoints WHERE 'task:'||task_id=scope_state.user_id AND revision<>processed)) ORDER BY due_at LIMIT 1`, m.Settings().Checkpoint).Scan(&user, &revision, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(user, "task:"), 10, 64)
	if err != nil {
		return err
	}
	mdl := m.ModelForTask(id)
	if mdl == nil {
		_, err := m.conn.ExecContext(ctx, `UPDATE scope_state SET retry_at=unixepoch()+30,error='等待可用的生成模型' WHERE app_name='scopeforge-task' AND user_id=? AND revision=?`, user, revision)
		return err
	}
	task, err := m.pg.GetTask(id)
	if err != nil {
		return err
	}
	if task == nil {
		_, err = m.conn.ExecContext(ctx, `UPDATE scope_state SET distilled_revision=revision,error='' WHERE app_name='scopeforge-task' AND user_id=?`, user)
		return err
	}
	entries, err := m.Task(id).ReadMemories(ctx, memory.UserKey{}, 1)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_, err = m.conn.ExecContext(ctx, `UPDATE scope_state SET distilled_revision=? WHERE app_name='scopeforge-task' AND user_id=? AND revision=?`, revision, user, revision)
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err = m.distill(callCtx, id, mdl, true)
	if err != nil {
		delay := time.Duration(1<<min(attempts, 6)) * time.Minute
		_, writeErr := m.conn.ExecContext(ctx, `UPDATE scope_state SET attempts=attempts+1,retry_at=?,error='提炼暂不可用，将在后台重试' WHERE app_name='scopeforge-task' AND user_id=? AND revision=?`, time.Now().Add(delay).Unix(), user, revision)
		return errors.Join(err, writeErr)
	}
	_, err = m.conn.ExecContext(ctx, `UPDATE scope_state SET distilled_revision=?,attempts=0,retry_at=0,error='' WHERE app_name='scopeforge-task' AND user_id=? AND revision=?`, revision, user, revision)
	return err
}
func (m *Memories) search(ctx context.Context, key memory.UserKey, q string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	if len(opts) == 0 {
		opts = []memory.SearchOption{memory.WithSearchOptions(memory.SearchOptions{Query: q, HybridSearch: true, Deduplicate: true})}
	}
	fallback := func() ([]*memory.Entry, error) { return m.store.SearchMemories(ctx, key, q, opts...) }
	options := memory.ResolveSearchOptions(q, opts)
	s := m.semanticIndex()
	if s == nil || !s.Status().Ready || strings.TrimSpace(q) == "" {
		return fallback()
	}
	var revision, synced int64
	var generation string
	if err := m.conn.QueryRowContext(ctx, `SELECT revision,synced_revision,generation FROM scope_state WHERE app_name=? AND user_id=?`, key.AppName, key.UserID).Scan(&revision, &synced, &generation); err != nil || revision != synced || generation != s.Status().Generation {
		return fallback()
	}
	semantic, err := s.Search(ctx, key, q, opts...)
	if err != nil {
		return fallback()
	}
	current, err := m.store.ReadMemories(ctx, key, 1000)
	if err != nil {
		return nil, err
	}
	valid := map[string]*memory.Entry{}
	for _, entry := range current {
		valid[entry.ID] = entry
	}
	out := []*memory.Entry{}
	for _, entry := range semantic {
		v := valid[entry.ID]
		if v != nil && v.Memory != nil && entry.Memory != nil && v.Memory.Memory == entry.Memory.Memory {
			copy := *v
			copy.Score = entry.Score
			out = append(out, &copy)
		}
	}
	limit := 8
	if options.MaxResults > 0 {
		limit = options.MaxResults
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type dynamicMemoryModel struct{ owner *Memories }

func (d dynamicMemoryModel) Info() model.Info { return model.Info{Name: "task-memory-extractor"} }
func (d dynamicMemoryModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	settings := d.owner.Settings()
	if !settings.TaskEnabled || !settings.AutoExtract {
		return nil, fmt.Errorf("automatic extraction is disabled")
	}
	mdl := d.owner.model()
	if mdl == nil {
		return nil, fmt.Errorf("memory extraction model is unavailable")
	}
	return mdl.GenerateContent(ctx, req)
}
