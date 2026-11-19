package llmpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/emhha/scopeforge/modelutil"
	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

// Member is one LLM profile in the chain, already built into a provider (wrapped
// with the recorder by the caller, so a failed attempt is still recorded under
// its own profile name).
type Member struct {
	ID       int64  // llm_profiles.id
	Name     string // profile name, for logs / UI
	Model    string
	Format   string // "anthropic" | "openai"
	Priority int    // the profile's configured priority (display only)
	Active   bool   // is_default (display only)
	// Rank is the ordering key the caller assigned: higher goes first, and members
	// sharing a Rank take turns leading (load-spreading across duplicate keys).
	// The caller encodes "active profile heads the chain" as a Rank above every
	// user-settable priority, so this type needs no policy of its own.
	Rank int
	// WindowTokens is the profile's context window in tokens. A member whose
	// window can't hold the request is skipped rather than made to fail on it.
	WindowTokens int
	Prov         llm.Model
}

// RankActive is the Rank the caller gives the chain head (the active profile, or
// an explicitly bound one) so it always outranks any configured priority.
const RankActive = int(^uint(0)>>1) - 1

// ErrExhausted is returned when every member of the chain failed.
var ErrExhausted = errors.New("LLM 轮询：所有配置均不可用")

// Pool is an llm.Model that fails over across an ordered chain of members.
// It is safe for concurrent use: members are immutable after construction and
// all mutable state lives in the shared Registry.
type Pool struct {
	members []*Member // in chain order (active first, then priority DESC)
	health  *Registry
	rr      atomic.Uint64 // rotates the starting point within an equal-priority group
}

// New builds a Pool over members (already in chain order). Returns nil when the
// chain is empty. A single-member chain is still a valid Pool — it just behaves
// exactly like the bare provider.
func New(members []*Member, health *Registry) *Pool {
	if len(members) == 0 {
		return nil
	}
	if health == nil {
		health = NewRegistry(nil, nil)
	}
	return &Pool{members: members, health: health}
}

// Members returns the chain in order (read-only).
func (p *Pool) Members() []*Member { return p.members }

// Head returns the first member of the chain.
func (p *Pool) Head() *Member { return p.members[0] }

// GenerateContent routes native model calls. Once output is visible, an error is terminal.
func (p *Pool) Info() llm.Info { return p.Head().Prov.Info() }
func (p *Pool) ProjectProviderData(data *llm.ProviderData) (llm.Message, error) {
	if data != nil {
		for _, member := range p.members {
			info := member.Prov.Info()
			if info.Name == data.Model && info.ProviderDataProtocol == data.Protocol {
				return modelutil.ProjectProviderData(member.Prov, data)
			}
		}
	}
	return llm.Message{}, fmt.Errorf("provider data does not belong to an LLM pool candidate")
}
func (p *Pool) GenerateContent(ctx context.Context, req *llm.Request) (<-chan *llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("nil model request")
	}
	out := make(chan *llm.Response)
	go func() {
		defer close(out)
		var last error
		for _, m := range p.order(*req) {
			callCtx, cancel := context.WithCancel(ctx)
			source, err := m.Prov.GenerateContent(callCtx, req)
			committed := false
			if err == nil && source != nil {
				for ev := range source {
					if ev == nil {
						continue
					}
					if ev.Error != nil && !committed {
						err = ev.Error
						cancel()
						continue
					}
					if err != nil {
						continue
					}
					if ResponseCommitsOutput(ev) {
						committed = true
					}
					if committed || ev.Done {
						select {
						case out <- ev:
						case <-ctx.Done():
							err = ctx.Err()
							cancel()
						}
					}
				}
			} else if err == nil {
				err = errors.New("model returned nil channel")
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				p.health.Pass(m.ID)
				return
			}
			last = err
			if committed || !shouldFailover(ctx, err) {
				out <- &llm.Response{Error: &llm.ResponseError{Type: llm.ErrorTypeAPIError, Message: err.Error()}, Done: true}
				return
			}
			p.health.Trip(m.ID, trimErr(err), isHardFailure(err))
		}
		if last == nil {
			last = ErrExhausted
		}
		select {
		case out <- &llm.Response{Error: &llm.ResponseError{Type: llm.ErrorTypeAPIError, Message: fmt.Sprintf("%v: %v", ErrExhausted, last)}, Done: true}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}
func ResponseCommitsOutput(ev *llm.Response) bool {
	if ev == nil {
		return false
	}
	for _, c := range ev.Choices {
		for _, m := range []llm.Message{c.Delta, c.Message} {
			if m.Content != "" || m.ReasoningContent != "" || len(m.ToolCalls) > 0 || len(m.ContentParts) > 0 {
				return true
			}
		}
	}
	return false
}

// order picks the members to try, in order: skip those in a cooling-off window
// and those whose context window can't hold this request, then rotate within each
// equal-priority group so same-priority profiles share the load. Never returns an
// empty slice — if everything is filtered out, the head of the chain is tried
// anyway, since stalling the engine is worse than one more failed request.
func (p *Pool) order(req llm.Request) []*Member {
	est := estimateTokens(req)
	var open []*Member
	for _, m := range p.members {
		if p.health.IsOpen(m.ID) {
			continue
		}
		if m.WindowTokens > 0 && est > m.WindowTokens {
			continue // would 400 on length — not a useful failover target
		}
		open = append(open, m)
	}
	if len(open) == 0 {
		return p.members[:1] // last resort: probe the head rather than stall
	}
	return rotateGroups(open, p.rr.Add(1)-1)
}

// rotateGroups rotates each run of equal-ranked members by n, so profiles sharing
// a priority take turns going first (free load-spreading across duplicate keys).
// The chain head has its own Rank and always stays at the front.
func rotateGroups(in []*Member, n uint64) []*Member {
	out := make([]*Member, 0, len(in))
	for i := 0; i < len(in); {
		j := i + 1
		for j < len(in) && in[j].Rank == in[i].Rank {
			j++
		}
		g := in[i:j]
		if len(g) > 1 {
			off := int(n % uint64(len(g)))
			for k := range g {
				out = append(out, g[(k+off)%len(g)])
			}
		} else {
			out = append(out, g...)
		}
		i = j
	}
	return out
}

// statusRe accepts the HTTP error formats emitted by the native provider SDKs.
var statusRe = regexp.MustCompile(`(?i)(?:status[=: ]+|: )(\d{3})\b`)

// HTTPStatus returns the HTTP status carried by a provider error, or zero.
func HTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

// shouldFailover reports whether err justifies trying the next profile.
//
// Never fails over on context cancellation — that's the user stopping a task or a
// task-level timeout, and burning a backup key on it would both waste credit and
// pollute the run's termination diagnosis. Never on 400 either: a malformed or
// over-long request fails identically everywhere.
func shouldFailover(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch code := HTTPStatus(err); {
	case code == 0:
		return true // no status → transport-level failure (reset / DNS / timeout)
	case code == 400:
		return false // bad or over-long request: identical everywhere
	case code == 401, code == 402, code == 403, code == 404, code == 408, code == 429:
		return true
	case code >= 500:
		return true
	}
	return false
}

// isHardFailure reports whether the failure is deterministic (the profile will
// keep failing until a human fixes it) rather than transient. Hard failures open
// the breaker on the first occurrence.
func isHardFailure(err error) bool {
	switch HTTPStatus(err) {
	case 401, 402, 403, 404:
		return true
	}
	return false
}

// trimErr shortens an error for logs/UI — provider bodies can be long.
func trimErr(err error) string {
	s := strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// estimateTokens roughly sizes a request so members whose context window clearly
// can't hold it are skipped. Deliberately crude (~3.5 chars/token) and biased to
// over-estimate slightly; it only needs to tell "fits" from "nowhere near".
func estimateTokens(req llm.Request) int {
	b, _ := json.Marshal(req.Messages)
	return len(b)/3 + len(req.Tools)*120
}
