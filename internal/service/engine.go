// Package service is the domain orchestrator. It turns normalized channel
// events into stored messages, commands, debounced answer jobs, model calls,
// formatted replies and outbound SMS — enforcing quotas, budgets, opt-outs
// and kill switches along the way.
package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/channel"
	"smsaighana/internal/config"
	"smsaighana/internal/formatting"
	"smsaighana/internal/grounding"
	"smsaighana/internal/observability"
	"smsaighana/internal/repository"
)

// maxQuestionRunes bounds the user text stored and sent to the model.
const maxQuestionRunes = 600

type Deps struct {
	Cfg      *config.Config
	Store    *repository.Store
	Registry *channel.Registry
	AI       *ai.Client
	Matcher  *grounding.Matcher
	Metrics  *observability.Metrics
	Log      *slog.Logger
	Now      func() time.Time
}

type Engine struct {
	Deps

	// ReconcileAfter is how old an uncertain send attempt must be before
	// the provider is asked about it.
	ReconcileAfter time.Duration

	flagMu     sync.Mutex
	flags      map[string]bool
	flagsAt    time.Time
	lastPurge  time.Time
	lastReload time.Time
}

func New(d Deps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = observability.NewMetrics()
	}
	if d.Matcher == nil {
		d.Matcher = grounding.NewMatcher(nil)
	}
	return &Engine{Deps: d, ReconcileAfter: 30 * time.Second, lastReload: time.Now()}
}

// flag reads a kill switch, cached for a few seconds.
func (e *Engine) flag(ctx context.Context, key string) bool {
	e.flagMu.Lock()
	defer e.flagMu.Unlock()
	if e.flags == nil || time.Since(e.flagsAt) > 3*time.Second {
		if f, err := e.Store.Q().Flags(ctx); err == nil {
			e.flags, e.flagsAt = f, time.Now()
		} else if e.flags == nil {
			e.Log.Error("loading system flags", "err", err)
			return true
		}
	}
	v, ok := e.flags[key]
	return !ok || v
}

// InvalidateFlags forces the next flag read to hit the database.
func (e *Engine) InvalidateFlags() {
	e.flagMu.Lock()
	e.flags = nil
	e.flagMu.Unlock()
}

func (e *Engine) today() time.Time { return repository.Day(e.Now()) }

func (e *Engine) enqueueOptions(debounce time.Duration, allowUCS2 bool) repository.EnqueueOptions {
	return repository.EnqueueOptions{
		Debounce:    debounce,
		Extend:      e.Cfg.CoalesceExtend,
		MaxCoalesce: e.Cfg.MaxCoalesce,
		MaxAttempts: e.Cfg.JobMaxAttempts,
		AllowUCS2:   allowUCS2,
	}
}

// moreSuffix is appended to an SMS page that has a continuation. Replies to
// USSD users come from an alphanumeric sender they cannot text back, so they
// are pointed back to the USSD menu instead of "Txt MORE".
func (e *Engine) moreSuffix(conv repository.Conversation) string {
	if conv.Channel == "ussd" {
		return " ..More: dial " + conv.ServiceID
	}
	return formatting.SMSMoreSuffix
}

// stripPagerSuffix removes our own continuation markers before a past
// answer is replayed to the model as history.
func stripPagerSuffix(s string) string {
	if i := strings.LastIndex(s, " .."); i > 0 && strings.Contains(strings.ToUpper(s[i:]), "MORE") {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// queueSMS stores an outbound SMS and a send job for it. NOTIFY inside the
// transaction wakes a worker as soon as it commits.
func (e *Engine) queueSMS(ctx context.Context, q *repository.Q, conv repository.Conversation, jobID, kind, source, body string) (string, error) {
	sender, err := e.Registry.SenderFor(conv.Provider)
	if err != nil {
		return "", err
	}
	st := formatting.Analyze(body)
	msgID, err := q.InsertOutbound(ctx, repository.OutboundMessage{
		ConversationID: conv.ID, JobID: jobID, Provider: sender.Name(), Kind: kind, Body: body,
		DeliveryChannel: "sms", Status: repository.StatusQueued, AnswerSource: source,
		Encoding: string(st.Encoding), Units: st.Units, Segments: st.Segments,
	})
	if err != nil {
		return "", err
	}
	if jobID == "" {
		if _, err := q.CreateSendJob(ctx, conv.ID, msgID, e.Cfg.JobMaxAttempts); err != nil {
			return "", err
		}
		if err := q.NotifyJobs(ctx); err != nil {
			return "", err
		}
	}
	return msgID, nil
}

// nextOverflowPage pops the next MORE page, sized for the given limits.
func (e *Engine) nextOverflowPage(ctx context.Context, q *repository.Q, convID string, lim formatting.Limits, suffix string) (string, bool, error) {
	o, ok, err := q.LockOverflow(ctx, convID)
	if err != nil || !ok {
		return "", false, err
	}
	var page, rest string
	if o.PagesLeft <= 1 {
		page = formatting.Truncate(o.Rest, lim)
	} else {
		page, rest = formatting.SplitFirst(o.Rest, lim, suffix)
	}
	if err := q.SetOverflow(ctx, convID, repository.Overflow{Rest: rest, PagesLeft: o.PagesLeft - 1}, e.Cfg.ContextTTL); err != nil {
		return "", false, err
	}
	e.Metrics.SMSMoreBufferServed.Inc()
	return page, true, nil
}

// splitForSMS produces the single-segment first page and the MORE remainder.
func (e *Engine) splitForSMS(conv repository.Conversation, text string) (string, repository.Overflow) {
	if e.Cfg.MaxMorePages <= 0 {
		return formatting.Truncate(text, formatting.SMSLimits), repository.Overflow{}
	}
	first, rest := formatting.SplitFirst(text, formatting.SMSLimits, e.moreSuffix(conv))
	return first, repository.Overflow{Rest: rest, PagesLeft: e.Cfg.MaxMorePages}
}

func backoff(attempt int) time.Duration {
	return time.Duration(attempt*attempt) * 5 * time.Second
}

var errAIUnavailable = errors.New("service: AI unavailable (kill switch, spend cap or circuit breaker)")
