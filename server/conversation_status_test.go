package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConversationListRunningState(t *testing.T) {
	s, _ := newRetestServer(t)
	var ids []int64
	for _, key := range []string{"auto", "reporter", "retester"} {
		c, err := s.m.pg.CreateConversation(key, "runtime status test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	t.Cleanup(func() {
		s.chatMu.Lock()
		clear(s.chatBusy)
		s.chatMu.Unlock()
		for _, id := range ids {
			_, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id=?1`, id)
		}
	})
	check := func(want map[int64]bool) {
		t.Helper()
		w := retestRequest(s.pgListConversations, http.MethodGet, 0, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var body struct {
			Conversations []conversationListItem `json:"conversations"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, item := range body.Conversations {
			if expected, ok := want[item.ID]; ok {
				found++
				if item.Running != expected || item.Title != "runtime status test" {
					t.Fatalf("conversation %d: running=%v want=%v title=%q", item.ID, item.Running, expected, item.Title)
				}
			}
		}
		if found != len(want) {
			t.Fatalf("found %d of %d conversations", found, len(want))
		}
	}
	check(map[int64]bool{ids[0]: false, ids[1]: false, ids[2]: false})
	s.chatMu.Lock()
	s.chatBusy[s.convBusyKey(ids[1])] = true
	s.chatBusy[s.convBusyKey(ids[2])] = true
	s.chatBusy["unrelated-task"] = true
	s.chatMu.Unlock()
	check(map[int64]bool{ids[0]: false, ids[1]: true, ids[2]: true})
	s.chatMu.Lock()
	clear(s.chatBusy)
	s.chatMu.Unlock()
	check(map[int64]bool{ids[0]: false, ids[1]: false, ids[2]: false})
}

func TestCompletedConversationTailDrainsAllExecutionEvents(t *testing.T) {
	s, _ := newRetestServer(t)
	conv, err := s.m.pg.CreateConversation("auto", "burst", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Insert in a transaction so this test exercises pagination rather than disk fsync speed.
	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 1003; i++ {
		if _, err = tx.Exec(`INSERT INTO conversation_activities(conversation_id,kind,summary) VALUES(?1,'text',?2)`, conv.ID, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	total := 0
	for page := 0; page < 3; page++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/?since=%d&limit=10", cursor), nil)
		req.SetPathValue("id", fmt.Sprint(conv.ID))
		w := httptest.NewRecorder()
		s.pgConversationMessages(w, req)
		var body struct {
			Items []struct {
				ID int64 `json:"seq"`
			} `json:"items"`
			Cursor  int64 `json:"cursor"`
			HasMore bool  `json:"hasMore"`
			Running bool  `json:"running"`
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Running {
			t.Fatal("fixture unexpectedly running")
		}
		for _, item := range body.Items {
			if item.ID <= cursor {
				t.Fatal("duplicate or reversed cursor")
			}
			cursor = item.ID
			total++
		}
		if body.Cursor != cursor {
			t.Fatal("cursor skipped events")
		}
		if !body.HasMore {
			break
		}
	}
	if total != 1003 {
		t.Fatalf("only loaded %d events after completion", total)
	}
}
