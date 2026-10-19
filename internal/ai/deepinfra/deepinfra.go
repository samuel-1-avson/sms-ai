// Package deepinfra calls DeepInfra's OpenAI-compatible chat completions
// API (plan §7.2): POST {base}/chat/completions with a Bearer token.
package deepinfra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"smsaighana/internal/ai"
)

type Adapter struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Adapter {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Adapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// Per-call deadlines come from the context; this is a backstop.
		http: &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}
}

type chatRequest struct {
	Model           string           `json:"model"`
	Messages        []ai.ChatMessage `json:"messages"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	Temperature     float32          `json:"temperature"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Stream          bool             `json:"stream"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int     `json:"prompt_tokens"`
		CompletionTokens        int     `json:"completion_tokens"`
		EstimatedCost           float64 `json:"estimated_cost"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (a *Adapter) Complete(ctx context.Context, req ai.ChatRequest) (ai.ChatResult, error) {
	body, err := json.Marshal(chatRequest{
		Model:           req.PreferredModel,
		Messages:        req.Messages,
		MaxTokens:       req.MaxOutputTokens,
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
	})
	if err != nil {
		return ai.ChatResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ai.ChatResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := a.http.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ai.ChatResult{Latency: latency}, context.Canceled
		}
		var nerr net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &nerr) && nerr.Timeout() {
			return ai.ChatResult{Latency: latency}, fmt.Errorf("%w: %v", ai.ErrTimeout, err)
		}
		return ai.ChatResult{Latency: latency}, fmt.Errorf("%w: %v", ai.ErrUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ai.ChatResult{Latency: latency}, fmt.Errorf("%w: reading body: %v", ai.ErrUpstream, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ai.ChatResult{Latency: latency}, statusError(resp.StatusCode, raw)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ai.ChatResult{Latency: latency}, fmt.Errorf("%w: decoding response: %v", ai.ErrUpstream, err)
	}
	res := ai.ChatResult{
		Model:            parsed.Model,
		InputTokens:      parsed.Usage.PromptTokens,
		OutputTokens:     parsed.Usage.CompletionTokens,
		EstimatedCostUSD: parsed.Usage.EstimatedCost,
		Latency:          latency,
	}
	if res.Model == "" {
		res.Model = req.PreferredModel
	}
	if d := parsed.Usage.CompletionTokensDetails; d != nil {
		res.ReasoningTokens = d.ReasoningTokens
	}
	if len(parsed.Choices) > 0 {
		res.Content = strings.TrimSpace(parsed.Choices[0].Message.Content)
		res.FinishReason = parsed.Choices[0].FinishReason
	}
	if res.Content == "" {
		// The Reasoning-Token Trap: tokens were spent but nothing visible came back.
		return res, fmt.Errorf("%w (finish_reason=%q, reasoning_tokens=%d)", ai.ErrEmptyResponse, res.FinishReason, res.ReasoningTokens)
	}
	return res, nil
}

func statusError(code int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d", ai.ErrUnauthorized, code)
	case code == http.StatusPaymentRequired:
		return fmt.Errorf("%w: HTTP %d", ai.ErrQuotaExhausted, code)
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: HTTP %d", ai.ErrRateLimited, code)
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		return fmt.Errorf("%w: HTTP %d", ai.ErrTimeout, code)
	case code >= 500:
		return fmt.Errorf("%w: HTTP %d: %s", ai.ErrUpstream, code, snippet)
	default:
		return fmt.Errorf("%w: HTTP %d: %s", ai.ErrBadRequest, code, snippet)
	}
}
