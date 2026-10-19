package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/channel"
	"smsaighana/internal/commands"
	"smsaighana/internal/repository"
)

// ProcessJob runs one claimed job. Expected failures are handled inside
// (retry scheduling, notices, refunds); a returned error is unexpected and
// the caller reschedules the job.
func (e *Engine) ProcessJob(ctx context.Context, job *repository.Job) error {
	var result string
	var err error
	switch job.Kind {
	case repository.JobAnswer:
		result, err = e.processAnswer(ctx, job)
	case repository.JobSend:
		result, err = e.processSend(ctx, job)
	default:
		result, err = "unknown_kind", e.Store.Q().FailJob(ctx, job.ID, "unknown_kind", job.Kind)
	}
	e.Metrics.JobsProcessed.WithLabelValues(job.Kind, result).Inc()
	return err
}

// RescheduleAfterError is used by the worker when ProcessJob errored.
func (e *Engine) RescheduleAfterError(ctx context.Context, job *repository.Job, cause error) error {
	if job.Attempts < job.MaxAttempts {
		return e.Store.Q().RetryJob(ctx, job.ID, backoff(job.Attempts), cause.Error())
	}
	return e.Store.Q().FailJob(ctx, job.ID, "error", cause.Error())
}

func (e *Engine) processSend(ctx context.Context, job *repository.Job) (string, error) {
	q := e.Store.Q()
	if job.ReplyMessageID == nil {
		return "missing_message", q.FailJob(ctx, job.ID, "missing_message", "send job without message")
	}
	result, err := e.Dispatch(ctx, *job.ReplyMessageID)
	if err != nil {
		if job.Attempts < job.MaxAttempts {
			return "retry", q.RetryJob(ctx, job.ID, backoff(job.Attempts), err.Error())
		}
		return "send_failed", q.FailJob(ctx, job.ID, "send_failed", err.Error())
	}
	return result, q.CompleteJob(ctx, job.ID, result)
}

func (e *Engine) processAnswer(ctx context.Context, job *repository.Job) (string, error) {
	q := e.Store.Q()
	conv, err := q.GetConversation(ctx, job.ConversationID)
	if err != nil {
		return "", err
	}
	user, err := q.GetUser(ctx, conv.UserID)
	if err != nil {
		return "", err
	}
	if job.ReplyMessageID != nil {
		// A previous attempt produced the answer; only delivery is left.
		return e.finishAnswerDelivery(ctx, job, *job.ReplyMessageID, "answered")
	}
	if user.OptedOut {
		return "opted_out", e.refundAndComplete(ctx, job, user.ID, "opted_out")
	}
	question, err := q.JobQuestion(ctx, job.ID)
	if err != nil {
		return "", err
	}
	if question == "" {
		return "empty", e.refundAndComplete(ctx, job, user.ID, "empty")
	}

	creditDay := e.today()
	if job.CreditSource == nil {
		source := ""
		err := e.Store.Tx(ctx, func(q *repository.Q) error {
			var err error
			if source, err = q.ConsumeCredit(ctx, user.ID, creditDay, e.Cfg.DailyPerNumberFreeCap); err != nil || source == "" {
				return err
			}
			return q.SetJobCredit(ctx, job.ID, source, creditDay)
		})
		if err != nil {
			return "", err
		}
		if source == "" {
			e.Metrics.DailyLimitReached.Inc()
			err := e.Store.Tx(ctx, func(q *repository.Q) error {
				if err := e.sendLimitNoticeOnce(ctx, q, user.ID, conv, creditDay); err != nil {
					return err
				}
				return q.CompleteJob(ctx, job.ID, "over_limit")
			})
			return "over_limit", err
		}
		job.CreditSource, job.CreditDay = &source, &creditDay
	}

	actx, cancel := context.WithTimeout(ctx, e.Cfg.LLMTimeout)
	ans, err := e.generate(actx, conv, job.ID, question, job.AllowUCS2)
	cancel()
	if err != nil {
		retryable := ai.IsRetryable(err) && !errors.Is(err, errAIUnavailable)
		if retryable && job.Attempts < job.MaxAttempts {
			e.Log.Warn("answer attempt failed; retrying", "job", job.ID, "attempt", job.Attempts, "err", err)
			return "retry", q.RetryJob(ctx, job.ID, backoff(job.Attempts), err.Error())
		}
		e.Log.Error("answer failed", "job", job.ID, "err", err)
		return e.failAnswer(ctx, job, user.ID, conv, err)
	}

	first, overflow := e.splitForSMS(conv, ans.Text)
	var msgID string
	err = e.Store.Tx(ctx, func(q *repository.Q) error {
		var err error
		if msgID, err = e.queueSMS(ctx, q, conv, job.ID, repository.KindAnswer, ans.Source, first); err != nil {
			return err
		}
		if err := q.SetOverflow(ctx, conv.ID, overflow, e.Cfg.ContextTTL); err != nil {
			return err
		}
		if err := q.LinkModelCalls(ctx, job.ID, msgID); err != nil {
			return err
		}
		return q.SetJobReply(ctx, job.ID, msgID)
	})
	if err != nil {
		return "", err
	}
	if conv.Channel == "ussd" {
		e.Metrics.USSDAsyncSMSFallback.Inc()
	}
	return e.finishAnswerDelivery(ctx, job, msgID, "answered_"+ans.Source)
}

func (e *Engine) finishAnswerDelivery(ctx context.Context, job *repository.Job, msgID, result string) (string, error) {
	q := e.Store.Q()
	sendResult, err := e.Dispatch(ctx, msgID)
	if err != nil {
		if job.Attempts < job.MaxAttempts {
			return "retry_send", q.RetryJob(ctx, job.ID, backoff(job.Attempts), err.Error())
		}
		return "send_failed", q.FailJob(ctx, job.ID, "send_failed", err.Error())
	}
	if sendResult != repository.StatusSubmitted {
		result += "_" + sendResult
	}
	return result, q.CompleteJob(ctx, job.ID, result)
}

func (e *Engine) refundAndComplete(ctx context.Context, job *repository.Job, userID, result string) error {
	return e.Store.Tx(ctx, func(q *repository.Q) error {
		if job.CreditSource != nil && job.CreditDay != nil {
			if err := q.RefundCredit(ctx, userID, *job.CreditSource, *job.CreditDay); err != nil {
				return err
			}
		}
		return q.CompleteJob(ctx, job.ID, result)
	})
}

// failAnswer refunds the question and sends at most one apology per user
// per day, so an outage cannot multiply SMS spend (plan §5.4).
func (e *Engine) failAnswer(ctx context.Context, job *repository.Job, userID string, conv repository.Conversation, cause error) (string, error) {
	result, text := "failed", commands.FailedText
	if errors.Is(cause, errAIUnavailable) {
		result, text = "ai_unavailable", commands.BusyText
	}
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		if job.CreditSource != nil && job.CreditDay != nil {
			if err := q.RefundCredit(ctx, userID, *job.CreditSource, *job.CreditDay); err != nil {
				return err
			}
		}
		first, err := q.MarkOutageNotice(ctx, userID, e.today())
		if err != nil {
			return err
		}
		if first {
			if _, err := e.queueSMS(ctx, q, conv, "", repository.KindNotice, "policy", text); err != nil {
				return err
			}
		}
		return q.FailJob(ctx, job.ID, result, cause.Error())
	})
	return result, err
}

// Dispatch submits one outbound SMS exactly once. The send intent is
// recorded before the provider call; a timeout leaves the message
// 'unknown_timeout' for reconciliation instead of resending (plan §5.4).
// It returns the resulting message status, or an error when a retry is safe.
func (e *Engine) Dispatch(ctx context.Context, msgID string) (string, error) {
	var info repository.DispatchInfo
	var attemptID string
	// outcome is set when the message will not be sent now; the transaction
	// still commits so a blocked/unknown status is persisted.
	outcome := ""
	day := e.today()
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		var err error
		if info, err = q.GetForDispatch(ctx, msgID, true); err != nil {
			return err
		}
		switch info.Status {
		case repository.StatusQueued:
		case repository.StatusSending:
			// A previous attempt crashed mid-send: outcome unknown, never resend blindly.
			outcome = repository.StatusUnknownTimeout
			return q.UpdateOutbound(ctx, msgID, repository.StatusUnknownTimeout, "", "crashed_mid_send")
		default:
			outcome = info.Status
			return nil
		}
		if !e.flag(ctx, repository.FlagSMSOutboundEnabled) {
			outcome = repository.StatusBlockedOff
			return q.UpdateOutbound(ctx, msgID, repository.StatusBlockedOff, "", "kill_switch")
		}
		segments := max(info.Segments, 1)
		ok, err := q.ReserveSegments(ctx, day, segments, e.Cfg.GlobalDailySMSSegmentCap, e.Cfg.SMSSegmentCostUSD, e.Cfg.GlobalDailySpendCapUSD)
		if err != nil {
			return err
		}
		if !ok {
			e.Log.Warn("global SMS cap reached; message not sent", "message", msgID)
			outcome = repository.StatusBlockedCap
			return q.UpdateOutbound(ctx, msgID, repository.StatusBlockedCap, "", "daily_cap")
		}
		if err := q.UpdateOutbound(ctx, msgID, repository.StatusSending, "", ""); err != nil {
			return err
		}
		attemptID, err = q.InsertSendAttempt(ctx, msgID, info.Provider, msgID)
		return err
	})
	if err != nil {
		return "", err
	}
	switch outcome {
	case "":
	case repository.StatusBlockedCap, repository.StatusBlockedOff, repository.StatusUnknownTimeout:
		e.Metrics.SMSOutboundFailed.WithLabelValues(info.Provider, outcome).Inc()
		return outcome, nil
	default:
		return outcome, nil // already handled by an earlier attempt
	}

	q := e.Store.Q()
	phone, err := q.UserPhone(ctx, info.UserID)
	if err != nil {
		return "", e.releaseAfterFailure(ctx, info, attemptID, day, err)
	}
	sender, err := e.Registry.SenderFor(info.Provider)
	if err != nil {
		return "", e.releaseAfterFailure(ctx, info, attemptID, day, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res, sendErr := sender.Send(sctx, channel.OutboundSMS{
		To: phone, SenderID: e.Cfg.SMSOutboundSenderID, Body: info.Body, Encoding: info.Encoding, ClientRef: msgID,
	})
	cancel()

	if sendErr == nil {
		if err := q.UpdateSendAttempt(ctx, attemptID, "submitted", res.ProviderMessageID, ""); err != nil {
			return "", err
		}
		if err := q.UpdateOutbound(ctx, msgID, repository.StatusSubmitted, res.ProviderMessageID, ""); err != nil {
			return "", err
		}
		e.Metrics.ObserveSubmitted(sender.Name(), info.Encoding, info.Segments)
		return repository.StatusSubmitted, nil
	}

	switch channel.SendErrorKindOf(sendErr) {
	case channel.SendUncertain:
		e.Log.Warn("SMS submission outcome unknown; will reconcile", "message", msgID, "err", sendErr)
		e.Metrics.SMSOutboundFailed.WithLabelValues(sender.Name(), "unknown_timeout").Inc()
		if err := q.UpdateSendAttempt(ctx, attemptID, "unknown_timeout", "", sendErr.Error()); err != nil {
			return "", err
		}
		return repository.StatusUnknownTimeout, q.UpdateOutbound(ctx, msgID, repository.StatusUnknownTimeout, "", "timeout")
	case channel.SendPermanent:
		e.Log.Error("SMS rejected by provider", "message", msgID, "err", sendErr)
		e.Metrics.SMSOutboundFailed.WithLabelValues(sender.Name(), "rejected").Inc()
		if err := e.Store.Tx(ctx, func(q *repository.Q) error {
			if err := q.UpdateSendAttempt(ctx, attemptID, "failed", "", sendErr.Error()); err != nil {
				return err
			}
			if err := q.ReleaseSegments(ctx, day, max(info.Segments, 1)); err != nil {
				return err
			}
			return q.UpdateOutbound(ctx, msgID, repository.StatusFailed, "", "rejected")
		}); err != nil {
			return "", err
		}
		return repository.StatusFailed, nil
	default:
		e.Metrics.SMSOutboundFailed.WithLabelValues(sender.Name(), "retryable").Inc()
		return "", e.releaseAfterFailure(ctx, info, attemptID, day, sendErr)
	}
}

// releaseAfterFailure puts a message back in the queue when the provider
// certainly did not accept it, returning the reserved segments.
func (e *Engine) releaseAfterFailure(ctx context.Context, info repository.DispatchInfo, attemptID string, day time.Time, cause error) error {
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		if err := q.UpdateSendAttempt(ctx, attemptID, "failed", "", cause.Error()); err != nil {
			return err
		}
		if err := q.ReleaseSegments(ctx, day, max(info.Segments, 1)); err != nil {
			return err
		}
		return q.UpdateOutbound(ctx, info.ID, repository.StatusQueued, "", "")
	})
	if err != nil {
		return err
	}
	return fmt.Errorf("dispatch: %w", cause)
}
