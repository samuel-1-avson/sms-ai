package ai

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"smsaighana/internal/config"
)

// MinReasoningMaxTokens leaves room for ~150 reasoning tokens plus a short
// visible answer when calling reasoning models (plan §7.2 rule 1).
const MinReasoningMaxTokens = 350

// CallRecord describes one model attempt for the model_calls ledger.
type CallRecord struct {
	Model           string
	Status          string // ok | empty | error
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	CostUSD         float64
	Latency         time.Duration
	Err             error
}

// Outcome is the result of Client.Answer: the text plus every attempt made.
type Outcome struct {
	Text  string
	Model string
	Calls []CallRecord
}

// Observer receives per-attempt telemetry (implemented by observability).
type Observer interface {
	ObserveAICall(model, status string, latency time.Duration)
	ObserveAIEmptyReasoning(model string)
}

type ClientOptions struct {
	Primary         string
	Fallback        string
	ReasoningEffort string
	MaxOutputTokens int
	Temperature     float64
	Prices          map[string]config.ModelPrice
	Breaker         *Breaker
	Observer        Observer
}

// Client wraps a ChatModel with the reasoning-token guard, a single
// fallback-model attempt, a circuit breaker and cost estimation.
type Client struct {
	model ChatModel
	opts  ClientOptions
}

func NewClient(model ChatModel, opts ClientOptions) *Client {
	if opts.Breaker == nil {
		opts.Breaker = NewBreaker(5, 30*time.Second, 5*time.Minute)
	}
	return &Client{model: model, opts: opts}
}

func (c *Client) Breaker() *Breaker { return c.opts.Breaker }

// Answer asks the primary model, then the fallback model once if the
// primary returned empty text or a retryable error and time remains.
func (c *Client) Answer(ctx context.Context, messages []ChatMessage) (Outcome, error) {
	if err := c.opts.Breaker.Allow(); err != nil {
		return Outcome{}, err
	}
	var out Outcome
	models := []string{c.opts.Primary}
	if c.opts.Fallback != "" && c.opts.Fallback != c.opts.Primary {
		models = append(models, c.opts.Fallback)
	}
	var lastErr error
	for i, model := range models {
		if i > 0 && !(errors.Is(lastErr, ErrEmptyResponse) || IsRetryable(lastErr)) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		res, err := c.model.Complete(ctx, c.request(model, messages))
		rec := c.record(model, res, err)
		out.Calls = append(out.Calls, rec)
		if c.opts.Observer != nil {
			c.opts.Observer.ObserveAICall(model, rec.Status, rec.Latency)
			if errors.Is(err, ErrEmptyResponse) && IsReasoningModel(model) {
				c.opts.Observer.ObserveAIEmptyReasoning(model)
			}
		}
		if ctx.Err() == nil {
			// A deadline imposed by the caller (e.g. the USSD fast path) says
			// nothing about provider health, so it does not count.
			c.opts.Breaker.Record(err)
		}
		if err == nil {
			out.Text, out.Model = res.Content, model
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return out, lastErr
}

func (c *Client) request(model string, messages []ChatMessage) ChatRequest {
	req := ChatRequest{
		Messages:        messages,
		MaxOutputTokens: c.opts.MaxOutputTokens,
		PreferredModel:  model,
		Temperature:     float32(c.opts.Temperature),
	}
	if IsReasoningModel(model) {
		req.ReasoningEffort = c.opts.ReasoningEffort
		if req.ReasoningEffort == "" {
			req.ReasoningEffort = "low"
		}
		if req.MaxOutputTokens < MinReasoningMaxTokens {
			req.MaxOutputTokens = MinReasoningMaxTokens
		}
	}
	return req
}

func (c *Client) record(model string, res ChatResult, err error) CallRecord {
	rec := CallRecord{
		Model:           model,
		Status:          "ok",
		InputTokens:     res.InputTokens,
		OutputTokens:    res.OutputTokens,
		ReasoningTokens: res.ReasoningTokens,
		Latency:         res.Latency,
		Err:             err,
	}
	switch {
	case errors.Is(err, ErrEmptyResponse):
		rec.Status = "empty"
	case err != nil:
		rec.Status = "error"
	}
	rec.CostUSD = res.EstimatedCostUSD
	if rec.CostUSD <= 0 {
		rec.CostUSD = EstimateCost(c.opts.Prices, model, res.InputTokens, res.OutputTokens)
	}
	return rec
}

// defaultPrices are planning figures in USD per 1M tokens. gpt-oss-20b is
// from the plan (§7.3); unknown models use a deliberately high price so
// spend caps err on the safe side. Override with LLM_PRICES.
var defaultPrices = map[string]config.ModelPrice{
	"openai/gpt-oss-20b": {InputPerM: 0.03, OutputPerM: 0.14},
}

var fallbackPrice = config.ModelPrice{InputPerM: 0.60, OutputPerM: 0.60}

// EstimateCost prices a call from token counts.
func EstimateCost(prices map[string]config.ModelPrice, model string, in, out int) float64 {
	p, ok := prices[model]
	if !ok {
		p, ok = defaultPrices[model]
	}
	if !ok {
		p = fallbackPrice
	}
	return float64(in)/1e6*p.InputPerM + float64(out)/1e6*p.OutputPerM
}

// Breaker is a small in-process circuit breaker. Credential or credit
// failures open it for a long cooldown; bursts of transient failures open
// it briefly.
type Breaker struct {
	mu            sync.Mutex
	threshold     int
	shortCooldown time.Duration
	longCooldown  time.Duration
	failures      int
	openUntil     time.Time
	reason        string
	now           func() time.Time
}

func NewBreaker(threshold int, short, long time.Duration) *Breaker {
	return &Breaker{threshold: threshold, shortCooldown: short, longCooldown: long, now: time.Now}
}

func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.now().Before(b.openUntil) {
		return ErrCircuitOpen
	}
	return nil
}

func (b *Breaker) Record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case err == nil:
		b.failures = 0
	case errors.Is(err, ErrQuotaExhausted) || errors.Is(err, ErrUnauthorized):
		b.openUntil, b.reason, b.failures = b.now().Add(b.longCooldown), err.Error(), 0
	case errors.Is(err, context.Canceled) || errors.Is(err, ErrBadRequest) || errors.Is(err, ErrEmptyResponse):
		// Caller cancelled, a request-specific problem, or an empty reasoning
		// answer (handled by the fallback model): not a provider outage.
	default:
		b.failures++
		if b.failures >= b.threshold {
			b.openUntil, b.reason, b.failures = b.now().Add(b.shortCooldown), err.Error(), 0
		}
	}
}

// State returns "closed" or "open: <reason>" for the admin API.
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.now().Before(b.openUntil) {
		return "open: " + strings.TrimPrefix(b.reason, "ai: ")
	}
	return "closed"
}
