package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/emhha/scopeforge/sidequestion"
	"github.com/google/uuid"
)

var ErrSideBusy = errors.New("当前会话已有旁路问题正在回答")
var ErrSideParentGone = errors.New("旁路父会话已删除或归档")

// These checkpoints are process-local. The DB wrapper only supplies parent
// lifecycle validation; no session/request contents are written to SQLite.
type sideState struct {
	snapshot   sidequestion.Snapshot
	generation int64
	requests   []sidequestion.Exchange
	memory     sidequestion.Memory
	updated    time.Time
}

func (d *DB) sideParent(ctx context.Context, p sidequestion.Parent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var id int64
	var err error
	if p.ConversationID > 0 {
		err = d.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id=?1`, p.ConversationID).Scan(&id)
	} else {
		err = d.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=?1 AND exploration_id=?2 AND deleted_at IS NULL AND archived_at IS NULL`, p.TaskID, p.ExplorationID).Scan(&id)
		if err == nil && p.IntentID > 0 {
			err = d.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE id=?1 AND exploration_id=?2 AND kind='intent' AND state NOT IN ('stopped','deleted')`, p.IntentID, p.ExplorationID).Scan(&id)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSideParentGone
	}
	return err
}
func (d *DB) sideState(ctx context.Context, key string) (*sideState, error) {
	state := d.sideSessions[key]
	if state == nil {
		return nil, ctx.Err()
	}
	if time.Since(state.updated) > 24*time.Hour {
		delete(d.sideSessions, key)
		return nil, nil
	}
	if err := d.sideParent(ctx, state.snapshot.Parent); err != nil {
		if errors.Is(err, ErrSideParentGone) {
			delete(d.sideSessions, key)
			return nil, nil
		}
		return nil, err
	}
	state.updated = time.Now()
	return state, nil
}
func (d *DB) SaveSideSnapshot(ctx context.Context, s sidequestion.Snapshot) error {
	if err := d.sideParent(ctx, s.Parent); err != nil {
		return err
	}
	request, err := sidequestion.CloneRequest(s.Request)
	if err != nil {
		return err
	}
	s.Request = request
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	if d.sideSessions == nil {
		d.sideSessions = map[string]*sideState{}
	}
	for key, value := range d.sideSessions {
		if time.Since(value.updated) > 24*time.Hour {
			delete(d.sideSessions, key)
		}
	}
	state := d.sideSessions[s.Parent.Key()]
	if state == nil {
		state = &sideState{generation: 1}
		d.sideSessions[s.Parent.Key()] = state
	}
	if s.RunID > state.snapshot.RunID || (s.RunID == state.snapshot.RunID && s.Version > state.snapshot.Version) {
		state.snapshot = s
	}
	state.updated = time.Now()
	return nil
}
func (d *DB) SideSnapshot(ctx context.Context, key string) (*sidequestion.Snapshot, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, key)
	if state == nil || err != nil {
		return nil, err
	}
	s := state.snapshot
	s.Request, err = sidequestion.CloneRequest(s.Request)
	return &s, err
}
func (d *DB) ExistingSideRequest(ctx context.Context, key, client string) (*sidequestion.Exchange, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, key)
	if state == nil || err != nil {
		return nil, err
	}
	for _, e := range state.requests {
		if e.ClientID == client && e.Generation == state.generation {
			copy := e
			return &copy, nil
		}
	}
	return nil, nil
}
func (d *DB) SideRequest(ctx context.Context, id string) (*sidequestion.Exchange, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	for key := range d.sideSessions {
		state, err := d.sideState(ctx, key)
		if err != nil {
			return nil, err
		}
		if state != nil {
			for _, e := range state.requests {
				if e.ID == id {
					copy := e
					return &copy, nil
				}
			}
		}
	}
	return nil, ctx.Err()
}
func (d *DB) CurrentSideRequest(ctx context.Context, key string) (*sidequestion.Exchange, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, key)
	if state == nil || err != nil {
		return nil, err
	}
	for _, e := range state.requests {
		if e.Running() {
			copy := e
			return &copy, nil
		}
	}
	return nil, nil
}
func (d *DB) SideHistory(ctx context.Context, key string, before int64, limit int) ([]sidequestion.Exchange, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	out := []sidequestion.Exchange{}
	state, err := d.sideState(ctx, key)
	if state == nil || err != nil {
		return out, err
	}
	for i := len(state.requests) - 1; i >= 0 && len(out) < limit; i-- {
		e := state.requests[i]
		if before == 0 || e.Ordinal < before {
			out = append(out, e)
		}
	}
	return out, nil
}
func (d *DB) SideReplay(ctx context.Context, key string) ([]sidequestion.Exchange, error) {
	items, err := d.SideHistory(ctx, key, 0, 20000)
	var out []sidequestion.Exchange
	for _, e := range items {
		if e.Status == "completed" {
			out = append(out, e)
			if len(out) == 20 {
				break
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}
func (d *DB) StartSideRequest(ctx context.Context, s sidequestion.Snapshot, clientID, question string) (*sidequestion.Exchange, bool, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, s.Parent.Key())
	if err != nil {
		return nil, false, err
	}
	if state == nil {
		return nil, false, ErrSideParentGone
	}
	for _, e := range state.requests {
		if e.ClientID == clientID && e.Generation == state.generation {
			if e.Question != question {
				return nil, false, fmt.Errorf("同一请求 ID 不能用于不同问题")
			}
			copy := e
			return &copy, false, nil
		}
	}
	for _, e := range state.requests {
		if e.Running() {
			return nil, false, ErrSideBusy
		}
	}
	d.sideOrdinal++
	e := sidequestion.Exchange{ID: uuid.NewString(), SessionKey: s.Parent.Key(), ClientID: clientID, Generation: state.generation, Question: question, Status: "running", Model: s.Model, SnapshotAt: s.CapturedAt, CreatedAt: time.Now().UTC(), Ordinal: d.sideOrdinal}
	state.requests = append(state.requests, e)
	return &e, true, nil
}
func (d *DB) UpdateSideRequest(ctx context.Context, e sidequestion.Exchange) (bool, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, e.SessionKey)
	if state == nil || err != nil {
		return false, err
	}
	for i, old := range state.requests {
		if old.ID == e.ID && old.Running() && old.Sequence < e.Sequence && old.Generation == state.generation && e.Generation == state.generation {
			e.Ordinal = old.Ordinal
			e.ClientID = old.ClientID
			e.CreatedAt = old.CreatedAt
			state.requests[i] = e
			return true, nil
		}
	}
	return false, nil
}
func (d *DB) ClearSideHistory(ctx context.Context, key string) error {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, key)
	if err != nil {
		return err
	}
	if state != nil {
		state.generation++
		state.requests = nil
		state.memory = sidequestion.Memory{}
	}
	return nil
}
func (d *DB) SideMemory(ctx context.Context, e sidequestion.Exchange) (sidequestion.Memory, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, e.SessionKey)
	if err != nil {
		return sidequestion.Memory{}, err
	}
	if state != nil && state.generation == e.Generation {
		for _, old := range state.requests {
			if old.ID == e.ID && old.Running() {
				return state.memory, nil
			}
		}
	}
	return sidequestion.Memory{}, sql.ErrNoRows
}
func (d *DB) SideReplayPage(ctx context.Context, e sidequestion.Exchange, after int64) ([]sidequestion.Exchange, error) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, e.SessionKey)
	var out []sidequestion.Exchange
	if state == nil || err != nil {
		return out, err
	}
	for _, item := range state.requests {
		if item.Generation == e.Generation && item.Status == "completed" && item.Ordinal > after && item.Ordinal < e.Ordinal {
			out = append(out, item)
			if len(out) == 20 {
				break
			}
		}
	}
	return out, nil
}
func (d *DB) SaveSideMemory(ctx context.Context, e sidequestion.Exchange, m sidequestion.Memory) error {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	state, err := d.sideState(ctx, e.SessionKey)
	if err != nil {
		return err
	}
	if state != nil && state.generation == e.Generation {
		for _, old := range state.requests {
			if old.ID == e.ID && old.Running() {
				state.memory = m
				return nil
			}
		}
	}
	return ErrSideParentGone
}
func (d *DB) InterruptSideRequests(ctx context.Context) error {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, state := range d.sideSessions {
		for i := range state.requests {
			e := &state.requests[i]
			if e.Running() {
				e.Status = "interrupted"
				e.Error = "回答已中断"
				e.Sequence++
			}
		}
	}
	return nil
}

func (d *DB) ForgetSideTask(taskID int64) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	for key, state := range d.sideSessions {
		if state.snapshot.Parent.TaskID == taskID {
			delete(d.sideSessions, key)
		}
	}
}
func (d *DB) ForgetSideConversation(id int64) {
	d.sideMu.Lock()
	defer d.sideMu.Unlock()
	for key, state := range d.sideSessions {
		if state.snapshot.Parent.ConversationID == id {
			delete(d.sideSessions, key)
		}
	}
}
