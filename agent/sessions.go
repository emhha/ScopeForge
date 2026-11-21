package agent

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	sessionsqlite "trpc.group/trpc-go/trpc-agent-go/session/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/session/summary"
)

type summaryModelKey struct{}

// NewSessionService is for isolated one-shot runs. Production uses OpenSessionService.
func NewSessionService() session.Service {
	return inmemory.NewSessionService(inmemory.WithSessionTTL(24*time.Hour), inmemory.WithSummarizer(sessionSummarizer()))
}

// OpenSessionService directly opens tRPC's native SQLite session backend. Native
// events, provider payloads, state and summaries share its own durable database.
func OpenSessionService(path string) (*sessionsqlite.Service, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	name := filepath.ToSlash(path)
	if len(name) > 1 && name[1] == ':' {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name}
	conn, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=10000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	svc, err := sessionsqlite.NewService(conn, sessionsqlite.WithEnableAsyncPersist(false), sessionsqlite.WithSessionEventLimit(0), sessionsqlite.WithSessionTTL(0), sessionsqlite.WithSoftDelete(false), sessionsqlite.WithSummarizer(sessionSummarizer()))
	if err != nil {
		conn.Close()
		return nil, err
	}
	return svc, nil
}

func sessionSummarizer() summary.SessionSummarizer {
	return summary.NewDynamicSummarizer(func(ctx context.Context, _ *session.Session) (summary.SessionSummarizer, error) {
		m, _ := ctx.Value(summaryModelKey{}).(model.Model)
		if m == nil {
			return nil, nil
		}
		window := m.Info().ContextWindow
		if window <= 0 {
			window = 200000
		}
		return summary.NewSummarizer(m, summary.WithTokenThreshold(window*2/3), summary.WithMaxSummaryWords(1500)), nil
	})
}
