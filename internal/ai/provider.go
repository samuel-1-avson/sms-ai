// Package ai defines the provider-neutral chat model contract (plan §7.1)
// and the orchestration around it: reasoning-token guards, model fallback,
// a circuit breaker and cost estimation.
package ai

import (
	"context"
	"errors"
	"strings"
	"time"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Messages        []ChatMessage
	MaxOutputTokens int
	PreferredModel  string
	ReasoningEffort string // "low" for reasoning models, "" otherwise
	Temperature     float32
}

type ChatResult struct {
	Content          string
	Model            string
	FinishReason     string
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	EstimatedCostUSD float64 // provider-reported cost when available
	Latency          time.Duration
}

// ChatModel is implemented by provider adapters. Complete may return a
// populated ChatResult together with an error (e.g. ErrEmptyResponse) so the
// tokens spent are still recorded.
type ChatModel interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResult, error)
}

var (
	// ErrEmptyResponse: the model returned no visible text — typically a
	// reasoning model that spent the whole token budget thinking (plan §7.2).
	ErrEmptyResponse  = errors.New("ai: empty response")
	ErrTimeout        = errors.New("ai: request timed out")
	ErrRateLimited    = errors.New("ai: rate limited")
	ErrQuotaExhausted = errors.New("ai: provider quota or credit exhausted")
	ErrUnauthorized   = errors.New("ai: provider rejected credentials")
	ErrUpstream       = errors.New("ai: upstream error")
	ErrBadRequest     = errors.New("ai: request rejected by provider")
	ErrCircuitOpen    = errors.New("ai: circuit breaker open")
)

// IsRetryable reports whether a later attempt may succeed.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrUpstream) || errors.Is(err, ErrEmptyResponse) || errors.Is(err, ErrCircuitOpen)
}

// IsReasoningModel identifies models that emit hidden chain-of-thought
// tokens and therefore need reasoning_effort and a larger token budget.
func IsReasoningModel(model string) bool {
	m := strings.ToLower(model)
	for _, marker := range []string{"gpt-oss", "deepseek-r1", "qwq", "-thinking", "o1-", "o3-", "o4-"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}
