package agent

import (
	"context"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/session/summary"
)

type summaryModelKey struct{}

// NewSessionService owns only ephemeral state, including SDK summaries and todos.
func NewSessionService() session.Service {
	return inmemory.NewSessionService(inmemory.WithSessionTTL(24*time.Hour), inmemory.WithSummarizer(summary.NewDynamicSummarizer(func(ctx context.Context, _ *session.Session) (summary.SessionSummarizer, error) {
		m, _ := ctx.Value(summaryModelKey{}).(model.Model)
		if m == nil {
			return nil, nil
		}
		window := m.Info().ContextWindow
		if window <= 0 {
			window = 200000
		}
		return summary.NewSummarizer(m, summary.WithTokenThreshold(window*2/3), summary.WithMaxSummaryWords(1500)), nil
	})))
}
