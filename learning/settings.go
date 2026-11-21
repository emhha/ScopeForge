package learning

import "github.com/emhha/scopeforge/db"

type MemorySettings struct {
	Checkpoint    bool `json:"checkpoint_enabled"`
	TaskEnabled   bool `json:"task_enabled"`
	GlobalEnabled bool `json:"global_enabled"`
	AutoDistill   bool `json:"auto_distill"`
	AutoExtract   bool `json:"auto_extract"`
}

func memorySettings(pg *db.DB) MemorySettings {
	return MemorySettings{Checkpoint: pg.GetBool("memory_checkpoint_enabled", true), TaskEnabled: pg.GetBool("memory_task_enabled", true), GlobalEnabled: pg.GetBool("memory_global_enabled", true), AutoDistill: pg.GetBool("memory_auto_distill", true), AutoExtract: pg.GetBool("memory_auto_extract", false)}
}
func (m *Memories) Settings() MemorySettings { return memorySettings(m.pg) }
func (m *Memories) Configure(c MemorySettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, err := m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, v := range map[string]bool{"memory_checkpoint_enabled": c.Checkpoint, "memory_task_enabled": c.TaskEnabled, "memory_global_enabled": c.GlobalEnabled, "memory_auto_distill": c.AutoDistill, "memory_auto_extract": c.AutoExtract} {
		text := "false"
		if v {
			text = "true"
		}
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?1,?2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, text); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = m.conn.Exec(`UPDATE memory_flags SET task_enabled=? WHERE id=1`, c.TaskEnabled)
	return err
}
