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

// USSD session states.
const (
	ussdQuestion = "question" // waiting for a question or a menu choice
	ussdAnswer   = "answer"   // an answer is on screen with its menu
	ussdOptedOut = "opted_out"
)

const (
	menuAnswerMore   = "\n1.Follow-up 2.Save to SMS\n3.More 0.Exit"
	menuAnswerNoMore = "\n1.Follow-up 2.Save to SMS\n0.Exit"
	promptQuestion   = "Type your question:"
	// Marks a USSD screen whose answer continues under "3.More".
	ussdMoreSuffix = " ..."
	ussdHelpText   = "Ask any question. Short answers show here; choose 2 to get one by SMS. AI can make mistakes. Chats kept max 30 days."
	// Extra screens of a long answer the user can page through inline.
	ussdMaxPages = 3
	// Margin before the worker may pick up a USSD job, so the fast path can
	// finish (or hand over) first.
	ussdHandoverMargin = 2 * time.Second
)

func con(msg string) channel.USSDReply { return channel.USSDReply{Message: msg} }
func end(msg string) channel.USSDReply { return channel.USSDReply{Message: msg, End: true} }

type ussdCtx struct {
	req  channel.USSDRequest
	user repository.User
	conv repository.Conversation
	sess repository.USSDSession
}

// HandleUSSD runs one step of a USSD session (plan §5.2). A question is
// answered inline when the grounding store or the model replies within the
// fast-path deadline (zero SMS cost); otherwise the session ends cleanly and
// the queued job delivers the answer by MT SMS.
func (e *Engine) HandleUSSD(ctx context.Context, req channel.USSDRequest) (channel.USSDReply, error) {
	from, err := msisdn.Normalize(req.FromE164)
	if err != nil {
		return channel.USSDReply{}, fmt.Errorf("%w: phone", channel.ErrBadPayload)
	}
	carrier := msisdn.Carrier(from)
	e.Metrics.ChannelInbound.WithLabelValues("ussd", req.Provider, carrier).Inc()
	serviceID := req.ServiceCode
	if serviceID == "" {
		serviceID = e.Cfg.USSDServiceCode
	}

	var u ussdCtx
	u.req = req
	isNew := false
	err = e.Store.Tx(ctx, func(q *repository.Q) error {
		var err error
		if u.user, _, err = q.FindOrCreateUser(ctx, from, carrier); err != nil {
			return err
		}
		if u.conv, err = q.TouchConversation(ctx, u.user.ID, serviceID, "ussd", req.Provider, e.Cfg.ContextTTL); err != nil {
			return err
		}
		var found bool
		if u.sess, found, err = q.GetUSSDSession(ctx, req.Provider, req.SessionID); err != nil {
			return err
		}
		if !found || req.NewSession {
			isNew = true
			u.sess = repository.USSDSession{Provider: req.Provider, SessionID: req.SessionID, UserID: u.user.ID,
				ConversationID: u.conv.ID, State: ussdQuestion}
		}
		return nil
	})
	if err != nil {
		return channel.USSDReply{}, err
	}

	var reply channel.USSDReply
	input := strings.TrimSpace(req.Input)
	switch {
	case isNew && u.user.OptedOut:
		u.sess.State = ussdOptedOut
		reply = con(commands.USSDOptedOutText + "\n1.Rejoin\n0.Exit")
	case isNew:
		reply = con(e.welcome(u.conv))
	case u.sess.State == ussdOptedOut:
		reply, err = e.ussdOptedOut(ctx, &u, input)
	case u.sess.State == ussdAnswer:
		reply, err = e.ussdAnswerMenu(ctx, &u, input)
	default:
		reply, err = e.ussdQuestionInput(ctx, &u, input)
	}
	if err != nil {
		return channel.USSDReply{}, err
	}
	if !reply.End {
		if err := e.Store.Q().SaveUSSDSession(ctx, u.sess, e.Cfg.USSDSessionTTL); err != nil {
			return channel.USSDReply{}, err
		}
	}
	return reply, nil
}

func (e *Engine) welcome(conv repository.Conversation) string {
	s := "SMS AI Ghana\n" + promptQuestion + "\n1.Help 2.New chat"
	if conv.HasOverflow {
		s += "\n3.Rest of last answer"
	}
	return s
}

func (e *Engine) ussdOptedOut(ctx context.Context, u *ussdCtx, input string) (channel.USSDReply, error) {
	if input != "1" {
		return end(commands.USSDGoodbyeText), nil
	}
	if err := e.Store.Q().SetOptOut(ctx, u.user.ID, false); err != nil {
		return channel.USSDReply{}, err
	}
	u.sess.State = ussdQuestion
	return con("Welcome back!\n" + e.welcome(u.conv)), nil
}

func (e *Engine) ussdQuestionInput(ctx context.Context, u *ussdCtx, input string) (channel.USSDReply, error) {
	cmd := commands.Parse(input)
	switch {
	case input == "":
		return con(commands.USSDEmptyQuestion + "\n" + promptQuestion), nil
	case input == "0":
		return end(commands.USSDGoodbyeText), nil
	case input == "1" || cmd.Kind == commands.Help:
		return con(ussdHelpText + "\n" + promptQuestion), nil
	case input == "2" || cmd.Kind == commands.New:
		if err := e.Store.Q().ResetContext(ctx, u.conv.ID); err != nil {
			return channel.USSDReply{}, err
		}
		u.sess.LastAnswer = ""
		return con("New chat started.\n" + promptQuestion), nil
	case input == "3" || cmd.Kind == commands.More:
		return e.ussdNextPage(ctx, u)
	case cmd.Kind == commands.Stop:
		if err := e.Store.Q().SetOptOut(ctx, u.user.ID, true); err != nil {
			return channel.USSDReply{}, err
		}
		return end(commands.StopText), nil
	case cmd.Kind == commands.Start:
		return con(e.welcome(u.conv)), nil
	case cmd.Kind == commands.Delete:
		if err := e.Store.Q().DeleteUserContent(ctx, u.user.ID); err != nil {
			return channel.USSDReply{}, err
		}
		return end(commands.DeleteText), nil
	case cmd.Kind == commands.Code:
		var text string
		err := e.Store.Tx(ctx, func(q *repository.Q) error {
			var err error
			text, err = e.redeem(ctx, q, u.user.ID, cmd.Arg, e.today())
			return err
		})
		if err != nil {
			return channel.USSDReply{}, err
		}
		return con(text + "\n" + promptQuestion), nil
	}
	return e.ussdAsk(ctx, u, input)
}

func (e *Engine) ussdAnswerMenu(ctx context.Context, u *ussdCtx, input string) (channel.USSDReply, error) {
	switch input {
	case "1":
		u.sess.State = ussdQuestion
		return con("Type your follow-up question:"), nil
	case "2":
		return e.ussdSaveToSMS(ctx, u)
	case "3":
		return e.ussdNextPage(ctx, u)
	case "0":
		return end(commands.USSDGoodbyeText), nil
	case "":
		return con("Choose 1, 2, 3 or 0.\n" + strings.TrimPrefix(menuAnswerMore, "\n")), nil
	}
	// Anything else is a new question typed straight from the menu.
	u.sess.State = ussdQuestion
	return e.ussdQuestionInput(ctx, u, input)
}

// ussdPage fits text plus the answer menu on one USSD screen.
func (e *Engine) ussdPage(text string) (page, rest string) {
	whole := e.Cfg.USSDMaxChars - len(menuAnswerNoMore)
	if formatting.Fits(text, formatting.Limits{GSM7: whole, UCS2: whole / 2}) {
		return text, ""
	}
	return formatting.SplitFirst(text, e.ussdScreenLimits(), ussdMoreSuffix)
}

func (e *Engine) ussdScreenLimits() formatting.Limits {
	budget := e.Cfg.USSDMaxChars - len(menuAnswerMore)
	return formatting.Limits{GSM7: budget, UCS2: budget / 2}
}

func answerScreen(page string, more bool) channel.USSDReply {
	if more {
		return con(page + menuAnswerMore)
	}
	return con(page + menuAnswerNoMore)
}

func (e *Engine) ussdNextPage(ctx context.Context, u *ussdCtx) (channel.USSDReply, error) {
	var page string
	var ok, more bool
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		var err error
		if page, ok, err = e.nextOverflowPage(ctx, q, u.conv.ID, e.ussdScreenLimits(), ussdMoreSuffix); err != nil || !ok {
			return err
		}
		_, more, err = q.LockOverflow(ctx, u.conv.ID)
		return err
	})
	if err != nil {
		return channel.USSDReply{}, err
	}
	if !ok {
		u.sess.State = ussdQuestion
		return con(commands.USSDNothingSavedText + "\n" + promptQuestion), nil
	}
	u.sess.State = ussdAnswer
	return answerScreen(page, more), nil
}

// ussdSaveToSMS sends the answer on screen to the user's SMS inbox.
func (e *Engine) ussdSaveToSMS(ctx context.Context, u *ussdCtx) (channel.USSDReply, error) {
	if u.sess.LastAnswer == "" {
		u.sess.State = ussdQuestion
		return con(commands.USSDNothingSavedText + "\n" + promptQuestion), nil
	}
	if !e.flag(ctx, repository.FlagSMSOutboundEnabled) {
		return end(commands.USSDSMSUnavailable), nil
	}
	first, overflow := e.splitForSMS(u.conv, u.sess.LastAnswer)
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		if _, err := e.queueSMS(ctx, q, u.conv, "", repository.KindSaved, "ussd_save", first); err != nil {
			return err
		}
		return q.SetOverflow(ctx, u.conv.ID, overflow, e.Cfg.ContextTTL)
	})
	if err != nil {
		return channel.USSDReply{}, err
	}
	// One save per answer, so the menu cannot be used to spam SMS.
	u.sess.LastAnswer = ""
	u.sess.State = ussdQuestion
	return end(commands.USSDSavedText), nil
}

// ussdAsk records the question, books a credit and a fallback job, then
// races the answer against the fast-path deadline.
func (e *Engine) ussdAsk(ctx context.Context, u *ussdCtx, input string) (channel.USSDReply, error) {
	scrubbed, _ := policy.Scrub(input)
	scrubbed = policy.TruncateQuestion(scrubbed, maxQuestionRunes)
	day := e.today()

	var jobID, source string
	var coalesced, overLimit, rateLimited bool
	err := e.Store.Tx(ctx, func(q *repository.Q) error {
		msgID, _, err := q.InsertInbound(ctx, repository.InboundMessage{
			ConversationID: u.conv.ID, Provider: u.req.Provider, Kind: repository.KindQuestion,
			Body: scrubbed, DeliveryChannel: "ussd", Status: repository.StatusReceived,
		})
		if err != nil {
			return err
		}
		if limit := e.Cfg.PerNumberRatePerMinute; limit > 0 {
			n, err := q.CountRecentInbound(ctx, u.user.ID, e.Now().Add(-time.Minute))
			if err != nil {
				return err
			}
			if n > limit {
				rateLimited = true
				return q.SetMessageStatus(ctx, msgID, repository.StatusRateLimited)
			}
		}
		ok, err := q.HasCredit(ctx, u.user.ID, day, e.Cfg.DailyPerNumberFreeCap)
		if err != nil {
			return err
		}
		if !ok {
			overLimit = true
			return q.SetMessageStatus(ctx, msgID, repository.StatusOverLimit)
		}
		// Not claimable until the fast path has had its chance.
		hold := e.Cfg.USSDFastPathTimeout + ussdHandoverMargin
		if jobID, coalesced, err = q.EnqueueAnswer(ctx, u.conv.ID, msgID, e.enqueueOptions(hold, false)); err != nil || coalesced {
			return err
		}
		if source, err = q.ConsumeCredit(ctx, u.user.ID, day, e.Cfg.DailyPerNumberFreeCap); err != nil {
			return err
		}
		return q.SetJobCredit(ctx, jobID, source, day)
	})
	if err != nil {
		return channel.USSDReply{}, err
	}
	switch {
	case rateLimited:
		return end("Too many messages. Please wait a minute and try again."), nil
	case overLimit:
		e.Metrics.DailyLimitReached.Inc()
		used, err := e.Store.Q().QuestionsUsed(ctx, u.user.ID, day)
		if err != nil {
			return channel.USSDReply{}, err
		}
		return end(commands.LimitReachedText(used, e.Cfg.DailyPerNumberFreeCap)), nil
	case coalesced:
		// An earlier question is still being answered by SMS; this one joins it.
		e.Metrics.SMSInboundCoalesced.Inc()
		return end(commands.USSDThinkingText), nil
	}

	fctx, cancel := context.WithTimeout(ctx, e.Cfg.USSDFastPathTimeout)
	ans, genErr := e.generate(fctx, u.conv, jobID, scrubbed, false)
	cancel()

	if genErr != nil {
		if errors.Is(genErr, errAIUnavailable) || !e.flag(ctx, repository.FlagSMSOutboundEnabled) {
			err := e.Store.Tx(ctx, func(q *repository.Q) error {
				if err := q.CancelPendingJob(ctx, jobID, "ai_unavailable"); err != nil {
					return err
				}
				return q.RefundCredit(ctx, u.user.ID, source, day)
			})
			if err != nil {
				return channel.USSDReply{}, err
			}
			return end(commands.BusyText), nil
		}
		// Too slow (or a transient error): hand over to the worker now.
		e.Log.Info("USSD fast path missed; answering by SMS", "job", jobID, "err", genErr)
		err := e.Store.Tx(ctx, func(q *repository.Q) error {
			if err := q.MakeAvailableNow(ctx, jobID); err != nil {
				return err
			}
			return q.NotifyJobs(ctx)
		})
		if err != nil {
			return channel.USSDReply{}, err
		}
		u.sess.State = ussdQuestion
		return end(commands.USSDThinkingText), nil
	}

	page, rest := e.ussdPage(ans.Text)
	err = e.Store.Tx(ctx, func(q *repository.Q) error {
		claimed, err := q.CompleteIfPending(ctx, jobID, "answered_inline_"+ans.Source)
		if err != nil {
			return err
		}
		if !claimed {
			e.Log.Warn("worker claimed USSD job before the inline answer was saved", "job", jobID)
		}
		st := formatting.Analyze(page)
		msgID, err := q.InsertOutbound(ctx, repository.OutboundMessage{
			ConversationID: u.conv.ID, JobID: jobID, Provider: u.req.Provider, Kind: repository.KindAnswer,
			Body: page, DeliveryChannel: "ussd", Status: repository.StatusDisplayed, AnswerSource: ans.Source,
			Encoding: string(st.Encoding), Units: st.Units, Segments: 0,
		})
		if err != nil {
			return err
		}
		if err := q.LinkModelCalls(ctx, jobID, msgID); err != nil {
			return err
		}
		return q.SetOverflow(ctx, u.conv.ID, repository.Overflow{Rest: rest, PagesLeft: ussdMaxPages}, e.Cfg.ContextTTL)
	})
	if err != nil {
		return channel.USSDReply{}, err
	}
	e.Metrics.USSDInlineDelivery.Inc()
	u.sess.State = ussdAnswer
	u.sess.LastAnswer = ans.Text
	return answerScreen(page, rest != ""), nil
}
