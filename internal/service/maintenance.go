package service

import (
	"context"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/grounding"
	"smsaighana/internal/repository"
)

// LoadGrounding optionally upserts seed snippets into the database, then
// loads the matcher from it. Outside development only reviewed snippets are
// served (unverified civic facts must never reach users).
func (e *Engine) LoadGrounding(ctx context.Context, seed []grounding.Snippet) error {
	q := e.Store.Q()
	if len(seed) > 0 {
		n, err := q.UpsertSnippets(ctx, seed)
		if err != nil {
			return err
		}
		e.Log.Info("grounding seed upserted", "snippets", n)
	}
	reviewedOnly := !e.Cfg.IsDevelopment() && e.Cfg.Env != "test"
	snippets, err := q.LoadSnippets(ctx, reviewedOnly)
	if err != nil {
		return err
	}
	e.Matcher.Replace(snippets)
	unreviewed := 0
	for _, s := range snippets {
		if !s.Reviewed {
			unreviewed++
		}
	}
	e.Log.Info("grounding store loaded", "snippets", len(snippets), "unreviewed", unreviewed, "reviewed_only", reviewedOnly)
	e.lastReload = time.Now()
	return nil
}

// Maintenance runs periodic housekeeping; the worker calls it every minute.
func (e *Engine) Maintenance(ctx context.Context) {
	q := e.Store.Q()
	if n, err := q.FailExhaustedJobs(ctx); err != nil {
		e.Log.Error("failing exhausted jobs", "err", err)
	} else if n > 0 {
		e.Log.Warn("jobs failed after exhausting attempts", "count", n)
	}
	e.ReconcileUncertainSends(ctx)

	if time.Since(e.lastPurge) > time.Hour {
		if r, err := q.Purge(ctx, e.Cfg.RetentionDays); err != nil {
			e.Log.Error("retention purge", "err", err)
		} else {
			e.lastPurge = time.Now()
			e.Log.Info("retention purge", "messages", r.Messages, "jobs", r.Jobs, "model_calls", r.ModelCalls,
				"ussd_sessions", r.USSDSessions, "overflow_buffers", r.Overflow)
		}
	}
	if e.Cfg.GroundingRefresh > 0 && time.Since(e.lastReload) > e.Cfg.GroundingRefresh {
		if err := e.LoadGrounding(ctx, nil); err != nil {
			e.Log.Error("reloading grounding store", "err", err)
		}
	}
	if segments, aiCost, err := q.DaySpend(ctx, e.today()); err == nil {
		e.Metrics.DailySpendUSD.Set(float64(segments)*e.Cfg.SMSSegmentCostUSD + aiCost)
		e.Metrics.DailySpendCapUSD.Set(e.Cfg.GlobalDailySpendCapUSD)
	}
}

// ReconcileUncertainSends resolves submissions whose outcome is unknown by
// asking the provider, when it supports lookups. Confirmed messages are
// marked submitted; messages the provider never saw are re-queued. Without
// a lookup API they stay 'unknown_timeout' for an operator to decide.
func (e *Engine) ReconcileUncertainSends(ctx context.Context) {
	q := e.Store.Q()
	attempts, err := q.OpenAttempts(ctx, "", e.ReconcileAfter, 50)
	if err != nil {
		e.Log.Error("listing uncertain sends", "err", err)
		return
	}
	for _, a := range attempts {
		sender, err := e.Registry.SenderFor(a.Provider)
		if err != nil {
			continue
		}
		querier, ok := sender.(channel.StatusQuerier)
		if !ok {
			continue
		}
		receipt, found, err := querier.QueryStatus(ctx, a.RequestID, a.ProviderMessageID)
		if err != nil {
			e.Log.Warn("provider status lookup failed", "attempt", a.ID, "err", err)
			continue
		}
		err = e.Store.Tx(ctx, func(q *repository.Q) error {
			if found {
				if err := q.UpdateSendAttempt(ctx, a.ID, "reconciled_submitted", receipt.ProviderMessageID, ""); err != nil {
					return err
				}
				return q.UpdateOutbound(ctx, a.MessageID, repository.StatusSubmitted, receipt.ProviderMessageID, "")
			}
			info, err := q.GetForDispatch(ctx, a.MessageID, true)
			if err != nil {
				return err
			}
			if err := q.UpdateSendAttempt(ctx, a.ID, "reconciled_not_found", "", ""); err != nil {
				return err
			}
			if err := q.ReleaseSegments(ctx, repository.Day(a.CreatedAt), max(info.Segments, 1)); err != nil {
				return err
			}
			if err := q.UpdateOutbound(ctx, a.MessageID, repository.StatusQueued, "", ""); err != nil {
				return err
			}
			if _, err := q.CreateSendJob(ctx, info.ConversationID, a.MessageID, e.Cfg.JobMaxAttempts); err != nil {
				return err
			}
			return q.NotifyJobs(ctx)
		})
		if err != nil {
			e.Log.Error("reconciling send attempt", "attempt", a.ID, "err", err)
		} else {
			e.Log.Info("reconciled uncertain send", "attempt", a.ID, "provider_has_it", found)
		}
	}
}
