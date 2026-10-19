package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/ai/prompt"
	"smsaighana/internal/formatting"
	"smsaighana/internal/repository"
)

type answer struct {
	Text    string // sanitized full answer
	Source  string // grounding | llm
	Snippet string
}

// generate answers a question: Local Ghana Grounding Store first (zero
// tokens), then the model with recent context and any partially matched
// verified facts. Model calls are recorded even when they fail.
func (e *Engine) generate(ctx context.Context, conv repository.Conversation, jobID, question string, allowUCS2 bool) (answer, error) {
	match := e.Matcher.Match(question)
	if match.Direct != nil {
		e.Metrics.GroundingHits.WithLabelValues(match.Direct.Category).Inc()
		return answer{Text: match.Direct.Answer, Source: "grounding", Snippet: match.Direct.Slug}, nil
	}
	e.Metrics.GroundingMisses.Inc()

	if !e.flag(ctx, repository.FlagAIEnabled) {
		return answer{}, fmt.Errorf("%w: ai_enabled=false", errAIUnavailable)
	}
	segments, aiCost, err := e.Store.Q().DaySpend(ctx, e.today())
	if err != nil {
		return answer{}, err
	}
	spend := float64(segments)*e.Cfg.SMSSegmentCostUSD + aiCost
	e.Metrics.DailySpendUSD.Set(spend)
	if spend >= e.Cfg.GlobalDailySpendCapUSD {
		e.Log.Warn("daily spend cap reached; AI calls paused", "spend_usd", spend, "cap_usd", e.Cfg.GlobalDailySpendCapUSD)
		return answer{}, fmt.Errorf("%w: daily spend cap", errAIUnavailable)
	}

	turns, err := e.Store.Q().RecentTurns(ctx, conv.ID, jobID, conv.ContextStartedAt, e.Cfg.ContextMaxTurns*2)
	if err != nil {
		return answer{}, err
	}
	var facts []string
	for _, s := range match.Context {
		facts = append(facts, s.Fact())
	}
	msgs := prompt.Build(prompt.Input{
		Question: question, History: toPromptTurns(turns), Facts: facts,
		Now: e.Now(), MaxInputTokens: e.Cfg.ContextMaxInputTokens,
	})

	out, err := e.AI.Answer(ctx, msgs)
	e.recordModelCalls(conv.Channel, jobID, out.Calls)
	if errors.Is(err, ai.ErrCircuitOpen) {
		return answer{}, fmt.Errorf("%w: %v", errAIUnavailable, err)
	}
	if err != nil {
		return answer{}, err
	}
	text := formatting.Sanitize(out.Text, formatting.SanitizeOptions{AllowUCS2: allowUCS2})
	if text == "" {
		return answer{}, fmt.Errorf("%w: nothing left after sanitizing", ai.ErrEmptyResponse)
	}
	return answer{Text: text, Source: "llm"}, nil
}

// recordModelCalls writes the token/cost ledger. It uses a fresh context so
// a USSD deadline that cancelled the call does not also lose its record.
func (e *Engine) recordModelCalls(channelType, jobID string, calls []ai.CallRecord) {
	if len(calls) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	total := 0.0
	q := e.Store.Q()
	for _, c := range calls {
		errMsg := ""
		if c.Err != nil {
			errMsg = c.Err.Error()
		}
		total += c.CostUSD
		if err := q.InsertModelCall(ctx, repository.ModelCall{
			JobID: jobID, Channel: channelType, Model: c.Model, Status: c.Status,
			InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, ReasoningTokens: c.ReasoningTokens,
			CostUSD: c.CostUSD, LatencyMS: int(c.Latency.Milliseconds()), Error: errMsg,
		}); err != nil {
			e.Log.Error("recording model call", "err", err)
		}
	}
	if err := q.AddAICost(ctx, e.today(), total); err != nil {
		e.Log.Error("recording AI cost", "err", err)
	}
}

// toPromptTurns strips pager suffixes and merges consecutive same-role turns
// (coalesced texts) so chat templates that require alternation stay valid.
func toPromptTurns(turns []repository.Turn) []prompt.Turn {
	var out []prompt.Turn
	for _, t := range turns {
		content := stripPagerSuffix(t.Content)
		if content == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == t.Role {
			out[n-1].Content += " " + content
			continue
		}
		out = append(out, prompt.Turn{Role: t.Role, Content: content})
	}
	return out
}
