package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/emhha/scopeforge/sidequestion"
	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

func sideFixture(t *testing.T) (*DB, sidequestion.Snapshot) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := d.CreateConversation("mainagent", "side persistence", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(c.ID) })
	s := sidequestion.Snapshot{Parent: sidequestion.Parent{ConversationID: c.ID}, RunID: 1, Version: 1, CapturedAt: time.Now().UTC(), Model: sidequestion.Model{Model: "fixture"}, Request: llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "main-only"}}}}
	if err = d.SaveSideSnapshot(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return d, s
}

func TestSideHistoryIdempotencyPagingAndRecovery(t *testing.T) {
	d, s := sideFixture(t)
	ctx := t.Context()
	first, created, err := d.StartSideRequest(ctx, s, "request-0", "question-0")
	if err != nil || !created {
		t.Fatalf("start %v %v", created, err)
	}
	again, created, err := d.StartSideRequest(ctx, s, "request-0", "question-0")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("dedup %v %v", created, err)
	}
	if _, _, err = d.StartSideRequest(ctx, s, "request-0", "different"); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	if _, _, err = d.StartSideRequest(ctx, s, "request-1", "question-1"); !errors.Is(err, ErrSideBusy) {
		t.Fatalf("busy %v", err)
	}
	for i := 0; i < 24; i++ {
		e := first
		if i > 0 {
			e, _, err = d.StartSideRequest(ctx, s, fmt.Sprintf("request-%d", i), fmt.Sprintf("question-%d", i))
			if err != nil {
				t.Fatal(err)
			}
		}
		e.Answer = fmt.Sprintf("answer-%d", i)
		e.Sequence = 1
		e.Status = "completed"
		if ok, err := d.UpdateSideRequest(ctx, *e); err != nil || !ok {
			t.Fatalf("finish %v %v", ok, err)
		}
	}
	page, err := d.SideHistory(ctx, s.Parent.Key(), 0, 20)
	if err != nil || len(page) != 20 {
		t.Fatalf("page %d %v", len(page), err)
	}
	tail, err := d.SideHistory(ctx, s.Parent.Key(), page[19].Ordinal, 20)
	if err != nil || len(tail) != 4 || tail[0].Ordinal >= page[19].Ordinal {
		t.Fatalf("tail %+v %v", tail, err)
	}
	replay, err := d.SideReplay(ctx, s.Parent.Key())
	if err != nil || len(replay) != 20 || replay[0].Question != "question-4" || replay[19].Question != "question-23" {
		t.Fatalf("replay %+v %v", replay, err)
	}
	e, _, err := d.StartSideRequest(ctx, s, "unfinished", "partial question")
	if err != nil {
		t.Fatal(err)
	}
	e.Answer = "saved partial"
	e.Sequence = 1
	e.Usage.InputTokens = 17
	if _, err = d.UpdateSideRequest(ctx, *e); err != nil {
		t.Fatal(err)
	}
	if err = d.InterruptSideRequests(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := d.SideRequest(ctx, e.ID)
	if err != nil || got.Status != "interrupted" || got.Answer != "saved partial" || got.Usage.InputTokens != 17 {
		t.Fatalf("recovery %+v %v", got, err)
	}
	saved, err := d.SideSnapshot(ctx, s.Parent.Key())
	if err != nil || saved.Request.Messages[0].Content != "main-only" {
		t.Fatalf("snapshot %+v %v", saved, err)
	}
	if _, _, err = d.StartSideRequest(ctx, *saved, "after-restart", "continue"); err != nil {
		t.Fatal(err)
	}
}

func TestSideMemoryPagingClearAndRestart(t *testing.T) {
	d, s := sideFixture(t)
	ctx := t.Context()
	var ordinal int64
	for i := 0; i < 50; i++ {
		e, _, err := d.StartSideRequest(ctx, s, fmt.Sprint(i), "history")
		if err != nil {
			t.Fatal(err)
		}
		e.Status = "completed"
		e.Sequence = 1
		e.Answer = "saved"
		if i == 2 {
			e.Status = "failed"
		}
		if _, err = d.UpdateSideRequest(ctx, *e); err != nil {
			t.Fatal(err)
		}
		if i == 29 {
			ordinal = e.Ordinal
		}
	}
	e, _, err := d.StartSideRequest(ctx, s, "admitted", "question")
	if err != nil {
		t.Fatal(err)
	}
	memory := sidequestion.Memory{History: "old decision", Through: ordinal, SnapshotKey: "snapshot", SnapshotSummary: "main evidence", TailStart: 3}
	if err = d.SaveSideMemory(ctx, *e, memory); err != nil {
		t.Fatal(err)
	}
	var all []sidequestion.Exchange
	for after := int64(0); ; {
		page, err := d.SideReplayPage(ctx, *e, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 20 {
			t.Fatal("unbounded page")
		}
		all = append(all, page...)
		after = page[len(page)-1].Ordinal
	}
	if len(all) != 49 {
		t.Fatalf("history missing/duplicated: %d", len(all))
	}
	if err = d.InterruptSideRequests(ctx); err != nil {
		t.Fatal(err)
	}
	next, _, err := d.StartSideRequest(ctx, s, "restart", "continue")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.SideMemory(ctx, *next)
	if err != nil || got != memory {
		t.Fatalf("memory after restart: %+v %v", got, err)
	}
	page, err := d.SideReplayPage(ctx, *next, memory.Through)
	if err != nil || len(page) != 20 || page[0].Ordinal <= ordinal {
		t.Fatalf("summary cursor: %+v %v", page, err)
	}
	if err = d.ClearSideHistory(ctx, s.Parent.Key()); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveSideMemory(ctx, *next, memory); !errors.Is(err, ErrSideParentGone) {
		t.Fatalf("late memory resurrected: %v", err)
	}
	fresh, _, err := d.StartSideRequest(ctx, s, "after-clear", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	got, err = d.SideMemory(ctx, *fresh)
	if err != nil || got != (sidequestion.Memory{}) {
		t.Fatalf("memory survived clear: %+v %v", got, err)
	}
	if snapshot, err := d.SideSnapshot(ctx, s.Parent.Key()); err != nil || snapshot.Request.Messages[0].Content != "main-only" {
		t.Fatal("memory changed main snapshot")
	}
}

func TestSideMemoryClearRace(t *testing.T) {
	d, s := sideFixture(t)
	e, _, err := d.StartSideRequest(t.Context(), s, "race", "question")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ClearSideHistory(t.Context(), s.Parent.Key()); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveSideMemory(t.Context(), *e, sidequestion.Memory{History: "late"}); !errors.Is(err, ErrSideParentGone) {
		t.Fatalf("late write=%v", err)
	}
	if row, err := d.SideRequest(t.Context(), e.ID); err != nil || row != nil {
		t.Fatalf("late request restored=%+v %v", row, err)
	}
}

func TestSideCheckpointsDoNotSurviveDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.CreateConversation("mainagent", "ephemeral", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := sidequestion.Snapshot{Parent: sidequestion.Parent{ConversationID: c.ID}, RunID: 1, Version: 1, Request: llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "private checkpoint"}}}}
	if err = d.SaveSideSnapshot(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.SideSnapshot(t.Context(), s.Parent.Key()); err != nil || got != nil {
		t.Fatalf("session persisted=%+v %v", got, err)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('side_question_sessions','side_question_requests')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session tables remain: %d %v", count, err)
	}
}

func TestSideClearLateWritersAndDeletedParent(t *testing.T) {
	d, s := sideFixture(t)
	ctx := t.Context()
	for i := 0; i < 10; i++ {
		e, _, err := d.StartSideRequest(ctx, s, "same-client", "question")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := d.ClearSideHistory(ctx, s.Parent.Key()); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			copy := *e
			copy.Sequence = 1
			copy.Answer = "late"
			copy.Status = "completed"
			if _, err := d.UpdateSideRequest(ctx, copy); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if row, err := d.SideRequest(ctx, e.ID); err != nil || row != nil {
			t.Fatalf("cleared answer resurrected: %+v %v", row, err)
		}
		e.Sequence = 2
		e.Status = "completed"
		if ok, err := d.UpdateSideRequest(ctx, *e); err != nil || ok {
			t.Fatalf("late update %v %v", ok, err)
		}
	}
	s.Version = 3
	if err := d.SaveSideSnapshot(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.Version = 2
	if err := d.SaveSideSnapshot(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := d.SideSnapshot(ctx, s.Parent.Key())
	if err != nil || got.Version != 3 {
		t.Fatalf("older version won: %+v %v", got, err)
	}
	if err = d.DeleteConversation(s.Parent.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveSideSnapshot(ctx, s); !errors.Is(err, ErrSideParentGone) {
		t.Fatalf("deleted parent restored: %v", err)
	}
	if got, err = d.SideSnapshot(ctx, s.Parent.Key()); err != nil || got != nil {
		t.Fatalf("delete cascade: %+v %v", got, err)
	}
}

func TestSideTaskArchiveVersions(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			d, err := Open(filepath.Join(t.TempDir(), "archive.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			task, err := d.CreateTask("goal", "description", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			s := sidequestion.Snapshot{Parent: sidequestion.Parent{TaskID: task.ID, ExplorationID: task.ExplorationID}, RunID: 1, Version: 1, Request: llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "private snapshot"}}}}
			if err = d.SaveSideSnapshot(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			_ = d.SetPaused(task.ID, true)
			job, err := d.QueueTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.ClaimTaskArchiveJob(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := d.SnapshotTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"side_question_sessions", "side_question_requests"} {
				if _, exists := snapshot.Tables[table]; exists {
					t.Fatal("archive contains model checkpoint")
				}
			}
			if err = d.CompleteTaskArchive(job.ID, snapshot, "/tmp/fixture.tar.zst", "fixture", 1, 1); err != nil {
				t.Fatal(err)
			}
			if got, err := d.SideSnapshot(t.Context(), s.Parent.Key()); err != nil || got != nil {
				t.Fatalf("archived session remains: %+v %v", got, err)
			}
			_, err = d.QueueTaskArchiveRestore(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.ClaimTaskArchiveJob(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			snapshot.FormatVersion = version
			if _, err = d.RestoreTaskArchive(job.ID, snapshot, 0); err != nil {
				t.Fatal(err)
			}
			if got, err := d.SideSnapshot(t.Context(), s.Parent.Key()); err != nil || got != nil {
				t.Fatalf("archive replayed checkpoint: %+v %v", got, err)
			}
		})
	}
}
