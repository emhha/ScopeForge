package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
)

type flowArtifacts struct {
	s   *Server
	run string
}

func (a flowArtifacts) Put(ctx context.Context, data []byte, media string, complete bool) (toolkit.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return toolkit.Artifact{}, err
	}
	if len(data) > 32<<20 {
		return toolkit.Artifact{}, fmt.Errorf("artifact exceeds 32 MiB")
	}
	id := uuid.NewString()
	dir := filepath.Join(a.s.m.dir, "flow-artifacts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return toolkit.Artifact{}, err
	}
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return toolkit.Artifact{}, err
	}
	out := toolkit.Artifact{ID: id, MediaType: media, Size: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Complete: complete}
	_, err := a.s.m.pg.Exec(`INSERT INTO flow_artifacts(id,run_id,media_type,size,sha256,complete) VALUES(?1,?2,?3,?4,?5,?6)`, id, a.run, media, len(data), out.SHA256, complete)
	if err != nil {
		_ = os.Remove(path)
	}
	return out, err
}
func (a flowArtifacts) Read(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var exists int
	if err := a.s.m.pg.QueryRow(`WITH RECURSIVE lineage(id,parent_run_id) AS (SELECT id,parent_run_id FROM flow_runs WHERE id=?2 UNION ALL SELECT r.id,r.parent_run_id FROM flow_runs r JOIN lineage ON r.id=lineage.parent_run_id), family(id) AS (SELECT id FROM lineage WHERE parent_run_id='' UNION ALL SELECT r.id FROM flow_runs r JOIN family ON r.parent_run_id=family.id) SELECT 1 FROM flow_artifacts WHERE id=?1 AND run_id IN(SELECT id FROM family)`, id, a.run).Scan(&exists); err != nil {
		return nil, fmt.Errorf("artifact is not owned by this run")
	}
	return os.ReadFile(filepath.Join(a.s.m.dir, "flow-artifacts", id))
}
func (s *Server) getFlowArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("artifact")
	if _, err := uuid.Parse(id); err != nil {
		writeErr(w, 404, "artifact not found")
		return
	}
	var media string
	if err := s.m.pg.QueryRow(`SELECT media_type FROM flow_artifacts WHERE id=?1`, id).Scan(&media); err != nil {
		writeErr(w, 404, "artifact not found")
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`"`)
	http.ServeFile(w, r, filepath.Join(s.m.dir, "flow-artifacts", id))
}

func (s *Server) listFlowArtifacts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.Query(`WITH RECURSIVE runs(id) AS(SELECT id FROM flow_runs WHERE id=?1 UNION ALL SELECT r.id FROM flow_runs r JOIN runs ON r.parent_run_id=runs.id) SELECT id,media_type,size,sha256,complete FROM flow_artifacts WHERE run_id IN(SELECT id FROM runs) ORDER BY created_at`, r.PathValue("run"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []toolkit.Artifact{}
	for rows.Next() {
		var a toolkit.Artifact
		if err := rows.Scan(&a.ID, &a.MediaType, &a.Size, &a.SHA256, &a.Complete); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, a)
	}
	writeJSON(w, 200, out)
}
