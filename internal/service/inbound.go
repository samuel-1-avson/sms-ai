package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/commands"
	"smsaighana/internal/formatting"
	"smsaighana/internal/msisdn"
	"smsaighana/internal/policy"
	"smsaighana/internal/repository"
)

// InboundResult reports what an inbound SMS triggered (for tests/logs).
type InboundResult struct {
	Duplicate bool
	Command   commands.Kind
	JobID     string
	Coalesced bool
	Dropped   string // rate_limited | opted_out | over_limit | empty
}

// HandleInboundSMS persists an inbound text and either runs a command or
// queues a debounced answer job. It does no model or provider I/O, so the
// webhook can be acknowledged well inside the 500 ms target.
func (e *Engine) HandleInboundSMS(ctx context.Context, ev channel.InboundSMS) (InboundResult, error) {
	var res InboundResult
	from, err := msisdn.Normalize(ev.FromE164)
	if err != nil {
		return res, fmt.Errorf("%w: sender number", channel.ErrBadPayload)
	}
	carrier := msisdn.Carrier(from)
	e.Metrics.ChannelInbound.WithLabelValues("sms", ev.Provider, carrier).Inc()

	body := strings.TrimSpace(ev.Body)
	if body == "" {
		res.Dropped = "empty"
		return res, nil
	}
	cmd := commands.Parse(body)
	res.Command = cmd.Kind
	scrubbed, _ := policy.Scrub(body)
	scrubbed = policy.TruncateQuestion(scrubbed, maxQuestionRunes)
	allowUCS2 := e.Cfg.AllowUCS2LocalLanguage && formatting.ContainsLocalLanguageLetters(body)
	serviceID := ev.ServiceID
	if serviceID == "" {
		serviceID = e.Cfg.SMSServiceID
	}
	day := e.today()

	err = e.Store.Tx(ctx, func(q *repository.Q) error {
		user, _, err := q.FindOrCreateUser(ctx, from, carrier)
		if err != nil {
			return err
		}
		conv, err := q.TouchConversation(ctx, user.ID, serviceID, "sms", ev.Provider, e.Cfg.ContextTTL)
		if err != nil {
			return err
		}
		kind := repository.KindQuestion
		if cmd.Kind != commands.None {
			kind = repository.KindCommand
		}
		msgID, dup, err := q.InsertInbound(ctx, repository.InboundMessage{
			ConversationID: conv.ID, Provider: ev.Provider, ProviderMessageID: ev.ProviderEventID,
			Kind: kind, Body: scrubbed, DeliveryChannel: "sms", Status: repository.StatusReceived,
		})
		if err != nil {
			return err
		}
		if dup {
			res.Duplicate = true
			return nil
		}

		if limit := e.Cfg.PerNumberRatePerMinute; limit > 0 {
			n, err := q.CountRecentInbound(ctx, user.ID, e.Now().Add(-time.Minute))
			if err != nil {
				return err
			}
			if n > limit {
				res.Dropped = "rate_limited"
				return q.SetMessageStatus(ctx, msgID, repository.StatusRateLimited)
			}
		}
		if user.OptedOut && cmd.Kind != commands.Start {
			res.Dropped = "opted_out"
			return q.SetMessageStatus(ctx, msgID, repository.StatusIgnored)
		}

		if cmd.Kind != commands.None {
			reply, replyKind, err := e.runCommand(ctx, q, user, conv, cmd, day, formatting.SMSLimits, e.moreSuffix(conv))
			if err != nil || reply == "" {
				return err
			}
			_, err = e.queueSMS(ctx, q, conv, "", replyKind, "command", reply)
			return err
		}

		ok, err := q.HasCredit(ctx, user.ID, day, e.Cfg.DailyPerNumberFreeCap)
		if err != nil {
			return err
		}
		if !ok {
			res.Dropped = "over_limit"
			e.Metrics.DailyLimitReached.Inc()
			if err := q.SetMessageStatus(ctx, msgID, repository.StatusOverLimit); err != nil {
				return err
			}
			return e.sendLimitNoticeOnce(ctx, q, user.ID, conv, day)
		}
		res.JobID, res.Coalesced, err = q.EnqueueAnswer(ctx, conv.ID, msgID, e.enqueueOptions(e.Cfg.InboundDebounce, allowUCS2))
		if res.Coalesced {
			e.Metrics.SMSInboundCoalesced.Inc()
		}
		return err
	})
	if res.Duplicate {
		e.Metrics.DuplicateInbound.Inc()
	}
	return res, err
}

func (e *Engine) sendLimitNoticeOnce(ctx context.Context, q *repository.Q, userID string, conv repository.Conversation, day time.Time) error {
	first, err := q.MarkLimitNotice(ctx, userID, day)
	if err != nil || !first {
		return err
	}
	used, err := q.QuestionsUsed(ctx, userID, day)
	if err != nil {
		return err
	}
	_, err = e.queueSMS(ctx, q, conv, "", repository.KindNotice, "policy", commands.LimitReachedText(used, e.Cfg.DailyPerNumberFreeCap))
	return err
}

// runCommand executes a keyword command and returns the reply text and the
// message kind to store it as. lim/suffix size MORE pages for the channel.
func (e *Engine) runCommand(ctx context.Context, q *repository.Q, user repository.User, conv repository.Conversation,
	cmd commands.Command, day time.Time, lim formatting.Limits, suffix string) (string, string, error) {
	reply := repository.KindCommandReply
	switch cmd.Kind {
	case commands.Help:
		return commands.HelpText, reply, nil
	case commands.New:
		return commands.NewChatText, reply, q.ResetContext(ctx, conv.ID)
	case commands.More:
		page, ok, err := e.nextOverflowPage(ctx, q, conv.ID, lim, suffix)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return commands.NoMoreText, reply, nil
		}
		return page, repository.KindMore, nil
	case commands.Stop:
		return commands.StopText, reply, q.SetOptOut(ctx, user.ID, true)
	case commands.Start:
		return commands.StartText, reply, q.SetOptOut(ctx, user.ID, false)
	case commands.Delete:
		return commands.DeleteText, reply, q.DeleteUserContent(ctx, user.ID)
	case commands.Code:
		text, err := e.redeem(ctx, q, user.ID, cmd.Arg, day)
		return text, reply, err
	}
	return "", "", nil
}

func (e *Engine) redeem(ctx context.Context, q *repository.Q, userID, code string, day time.Time) (string, error) {
	failed, err := q.FailedCodeAttempts(ctx, userID, day)
	if err != nil {
		return "", err
	}
	if failed >= e.Cfg.MaxFailedCodeAttempts {
		return commands.CodeTooManyText, nil
	}
	granted, balance, err := q.RedeemVoucher(ctx, userID, code)
	switch {
	case err == nil:
		e.Metrics.VoucherSegmentsRedeemed.Add(float64(granted))
		return commands.CodeAcceptedText(granted, balance), nil
	case errors.Is(err, repository.ErrVoucherInvalid):
		return commands.CodeInvalidText, q.IncrFailedCode(ctx, userID, day)
	case errors.Is(err, repository.ErrVoucherExpired), errors.Is(err, repository.ErrVoucherExhausted):
		return commands.CodeExpiredText, nil
	case errors.Is(err, repository.ErrVoucherUsed):
		return commands.CodeUsedText, nil
	}
	return "", err
}

// HandleDeliveryReceipts applies carrier receipts idempotently.
func (e *Engine) HandleDeliveryReceipts(ctx context.Context, receipts []channel.DeliveryReceipt) error {
	for _, r := range receipts {
		status := normalizeReceiptStatus(r.Status)
		if status == "" {
			continue
		}
		if _, err := e.Store.Q().ApplyReceipt(ctx, r.Provider, r.ProviderMessageID, status, r.ErrorCode); err != nil {
			return err
		}
	}
	return nil
}

func normalizeReceiptStatus(s string) string {
	switch strings.ToLower(s) {
	case "delivered", "delivrd", "success":
		return repository.StatusDelivered
	case "failed", "undelivered", "undeliv", "rejected", "rejectd", "blacklisted":
		return repository.StatusFailed
	case "expired":
		return "expired"
	case "submitted", "sent", "accepted", "enroute", "queued":
		return repository.StatusSubmitted
	}
	return ""
}
