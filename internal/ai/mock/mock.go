// Package mock is a deterministic stand-in for DeepInfra used in local
// development, the simulator and tests (LLM_PROVIDER=mock).
//
// Behaviour hooks, matched case-insensitively in the latest user message:
//   - "slow"  -> waits SlowLatency (exercises the USSD -> SMS fallback)
//   - "long"  -> returns a ~300 character answer (exercises MORE)
//   - "empty" -> returns empty content (exercises the reasoning-token guard)
//   - "fail"  -> returns an upstream error
package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"smsaighana/internal/ai"
)

type Model struct {
	Latency     time.Duration
	SlowLatency time.Duration

	calls atomic.Int64
	mu    sync.Mutex
	last  ai.ChatRequest
	// Respond overrides the canned behaviour when set (tests).
	Respond func(req ai.ChatRequest) (string, error)
}

func New(latency time.Duration) *Model {
	return &Model{Latency: latency, SlowLatency: 4500 * time.Millisecond}
}

// Calls returns how many completions were requested.
func (m *Model) Calls() int64 { return m.calls.Load() }

// LastRequest returns the most recent request (tests).
func (m *Model) LastRequest() ai.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *Model) Complete(ctx context.Context, req ai.ChatRequest) (ai.ChatResult, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.last = req
	m.mu.Unlock()

	question, history := "", 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			if question == "" {
				question = req.Messages[i].Content
			} else {
				history++
			}
		}
	}
	lower := strings.ToLower(question)

	delay := m.Latency
	if strings.Contains(lower, "slow") {
		delay = m.SlowLatency
	}
	start := time.Now()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return ai.ChatResult{Latency: time.Since(start)}, fmt.Errorf("%w: %v", ai.ErrTimeout, ctx.Err())
		}
		return ai.ChatResult{Latency: time.Since(start)}, ctx.Err()
	}

	in := 0
	for _, msg := range req.Messages {
		in += len(msg.Content)/4 + 4
	}
	res := ai.ChatResult{Model: req.PreferredModel, InputTokens: in, Latency: time.Since(start), FinishReason: "stop"}

	var content string
	var err error
	switch {
	case m.Respond != nil:
		content, err = m.Respond(req)
	case strings.Contains(lower, "fail"):
		err = fmt.Errorf("%w: mock failure", ai.ErrUpstream)
	case strings.Contains(lower, "empty"):
		res.FinishReason = "length"
		res.ReasoningTokens = req.MaxOutputTokens
		res.OutputTokens = req.MaxOutputTokens
		return res, fmt.Errorf("%w (mock)", ai.ErrEmptyResponse)
	case strings.Contains(lower, "photosynthesis"):
		// Curly quotes and an em dash on purpose: the GSM-7 sanitizer must fix them.
		content = "Sure! Photosynthesis is how plants use sunlight, water and carbon dioxide to make food — and it’s why we have oxygen."
	case strings.Contains(lower, "sunlight") && history > 0:
		content = "Sunlight supplies the energy plants need to turn water and carbon dioxide into sugars."
	case strings.Contains(lower, "long"):
		content = "Here is the answer: " + strings.Repeat("This mock answer is deliberately long so that the MORE buffer is exercised. ", 4)
	default:
		content = fmt.Sprintf("Mock answer (%d earlier turns): you asked %q. Set LLM_PROVIDER=deepinfra for real answers.", history, truncate(question, 60))
	}
	if err != nil {
		return res, err
	}
	res.Content = content
	res.OutputTokens = len(content)/4 + 1
	return res, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
