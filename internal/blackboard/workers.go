package blackboard

import (
	"database/sql"
	"time"
)

// ------------------------------------------------------------------ Worker 心跳

// CreateWorker 登记一个 Worker。
func (b *Blackboard) CreateWorker(w *Worker) error {
	_, err := b.db.Exec(`INSERT INTO workers (id, challenge_id, worker_type, phase, provider, model, session_id, status, handoff, intent_id, last_progress_at, has_correct_submission, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?)`,
		w.ID, w.ChallengeID, w.WorkerType, w.Phase, w.Provider, w.Model, w.SessionID, w.Handoff,
		nullIfZero(w.IntentID), w.LastProgressAt, boolInt(w.HasCorrectSubmission), time.Now().Unix())
	return err
}

// WorkerHeartbeat 更新心跳(进度时间戳)。
func (b *Blackboard) WorkerHeartbeat(id string, hasCorrect bool) error {
	_, err := b.db.Exec(`UPDATE workers SET last_progress_at=?, has_correct_submission=CASE WHEN ?=1 OR has_correct_submission=1 THEN 1 ELSE 0 END WHERE id=?`,
		time.Now().Unix(), boolInt(hasCorrect), id)
	return err
}

// MarkWorkerFinished 结束 Worker(running → 终态)。
func (b *Blackboard) MarkWorkerFinished(id, status string) error {
	_, err := b.db.Exec(`UPDATE workers SET status=?, finished_at=? WHERE id=?`,
		status, time.Now().Unix(), id)
	return err
}

// MarkWorkerFinishedIfRunning 条件终态:仅当仍为 running 时更新
// (避免 Abort 覆盖 runWorker 已写入的 done/failed)。
func (b *Blackboard) MarkWorkerFinishedIfRunning(id, status string) error {
	_, err := b.db.Exec(`UPDATE workers SET status=?, finished_at=? WHERE id=? AND status='running'`,
		status, time.Now().Unix(), id)
	return err
}

// Workers 读取 Worker 列表(按状态过滤)。
func (b *Blackboard) Workers(challengeID string, statuses ...string) ([]Worker, error) {
	q := `SELECT id, challenge_id, worker_type, phase, provider, model, session_id, status, handoff, intent_id, last_progress_at, has_correct_submission, created_at, finished_at
		FROM workers WHERE challenge_id = ?`
	args := []any{challengeID}
	if len(statuses) > 0 {
		q += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, s := range statuses {
			args = append(args, s)
		}
	}
	q += ` ORDER BY created_at ASC`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		var w Worker
		var intent sql.NullInt64
		var fin sql.NullInt64
		if err := rows.Scan(&w.ID, &w.ChallengeID, &w.WorkerType, &w.Phase, &w.Provider, &w.Model, &w.SessionID,
			&w.Status, &w.Handoff, &intent, &w.LastProgressAt, &w.HasCorrectSubmission, &w.CreatedAt, &fin); err != nil {
			return nil, err
		}
		if intent.Valid {
			w.IntentID = intent.Int64
		}
		if fin.Valid {
			w.FinishedAt = fin.Int64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
