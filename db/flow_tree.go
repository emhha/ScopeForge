package db

const flowRunTree = `WITH RECURSIVE runs(id) AS (SELECT id FROM flow_runs WHERE id=?1 UNION ALL SELECT r.id FROM flow_runs r JOIN runs ON r.parent_run_id=runs.id) `

func (d *DB) FlowTreeEvents(root string, since int64) ([]FlowRunEvent, error) {
	rows, err := d.Query(flowRunTree+`SELECT id,run_id,object,author,error,created_at,payload FROM flow_run_events WHERE run_id IN(SELECT id FROM runs) AND id>?2 ORDER BY id LIMIT 500`, root, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowRunEvent{}
	for rows.Next() {
		var e FlowRunEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.Object, &e.Author, &e.Error, &e.CreatedAt, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (d *DB) RunningFlowChildren(root string) ([]string, error) {
	rows, err := d.Query(flowRunTree+`SELECT id FROM flow_runs WHERE id IN(SELECT id FROM runs) AND id<>?1 AND status='running'`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
