package db

import "fmt"

const taskWorkflowTree = `WITH RECURSIVE owned(id) AS (
 SELECT id FROM flow_runs WHERE task_id=?1
 UNION SELECT r.id FROM flow_runs r JOIN owned ON r.parent_run_id=owned.id
) `

func taskWorkflowArtifacts(tx *Tx, taskID int64) ([]string, error) {
	var active int
	if err := tx.QueryRow(taskWorkflowTree+`SELECT count(*) FROM flow_runs WHERE id IN(SELECT id FROM owned) AND status='running'`, taskID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, fmt.Errorf("task has running workflows; cancel and drain before deleting")
	}
	rows, err := tx.Query(taskWorkflowTree+`SELECT id FROM flow_artifacts WHERE run_id IN(SELECT id FROM owned)`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func deleteTaskWorkflows(tx *Tx, taskID int64) error {
	if _, err := tx.Exec(taskWorkflowTree+`DELETE FROM flow_artifacts WHERE run_id IN(SELECT id FROM owned)`, taskID); err != nil {
		return err
	}
	_, err := tx.Exec(taskWorkflowTree+`DELETE FROM flow_runs WHERE id IN(SELECT id FROM owned)`, taskID)
	return err
}
