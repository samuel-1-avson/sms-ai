package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/ai/deepinfra"
	"smsaighana/internal/ai/prompt"
)

// fakeDeepInfra records requests and replies per model.
type fakeDeepInfra struct {
	mu       sync.Mutex
	requests []map[string]any
	reply    func(model string) (int, string)
}

func (f *fakeDeepInfra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/openai/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, "bad path or auth", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	code, payload := f.reply(req["model"].(string))
	w.WriteHeader(code)
	_, _ = io.WriteString(w, payload)
}

func completion(content string, reasoning int) string {
	b, _ := json.Marshal(map[string]any{
		"model": "x",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": map[bool]string{true: "stop", false: "length"}[content != ""],
		}},
		"usage": map[string]any{
			"prompt_tokens": 120, "completion_tokens": 300,
			"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		},
	})
	return string(b)
}

func newClient(srv *httptest.Server, primary, fallback string, maxTokens int) *ai.Client {
	return ai.NewClient(deepinfra.New(srv.URL+"/v1/openai", "test-key"), ai.ClientOptions{
		Primary: primary, Fallback: fallback, ReasoningEffort: "low", MaxOutputTokens: maxTokens, Temperature: 0.2,
	})
}

func TestReasoningGuardAppliedOnlyToReasoningModels(t *testing.T) {
	fake := &fakeDeepInfra{reply: func(string) (int, string) { return 200, completion("Ohm's law: V = I x R.", 40) }}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	for _, model := range []string{"openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo"} {
		c := newClient(srv, model, "", 100)
		if _, err := c.Answer(context.Background(), []ai.ChatMessage{{Role: "user", Content: "Ohm?"}}); err != nil {
			t.Fatal(err)
		}
	}
	gpt, llama := fake.requests[0], fake.requests[1]
	if gpt["reasoning_effort"] != "low" || gpt["max_tokens"].(float64) < ai.MinReasoningMaxTokens {
		t.Fatalf("gpt-oss request missing guard: %v", gpt)
	}
	if _, ok := llama["reasoning_effort"]; ok || llama["max_tokens"].(float64) != 100 {
		t.Fatalf("instruct model must not get reasoning_effort: %v", llama)
	}
}

func TestEmptyReasoningResponseFallsBack(t *testing.T) {
	fake := &fakeDeepInfra{reply: func(model string) (int, string) {
		if model == "openai/gpt-oss-20b" {
			return 200, completion("", 350) // the Reasoning-Token Trap
		}
		return 200, completion("Photosynthesis makes food from light.", 0)
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := newClient(srv, "openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo", 300)
	out, err := c.Answer(context.Background(), []ai.ChatMessage{{Role: "user", Content: "photosynthesis?"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Photosynthesis makes food from light." || len(out.Calls) != 2 {
		t.Fatalf("fallback not used: %+v", out)
	}
	if out.Calls[0].Status != "empty" || out.Calls[0].ReasoningTokens != 350 || out.Calls[0].CostUSD <= 0 {
		t.Fatalf("empty call must still be recorded with cost: %+v", out.Calls[0])
	}
}

func TestStatusErrorsAndBreaker(t *testing.T) {
	fake := &fakeDeepInfra{reply: func(string) (int, string) { return 402, `{"error":"insufficient balance"}` }}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := newClient(srv, "meta-llama/Llama-3.3-70B-Instruct-Turbo", "openai/gpt-oss-20b", 300)
	_, err := c.Answer(context.Background(), []ai.ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ai.ErrQuotaExhausted) {
		t.Fatalf("want ErrQuotaExhausted, got %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("quota exhaustion must not trigger the fallback model (%d requests)", len(fake.requests))
	}
	if _, err := c.Answer(context.Background(), nil); !errors.Is(err, ai.ErrCircuitOpen) {
		t.Fatalf("breaker should be open, got %v", err)
	}
	if !strings.HasPrefix(c.Breaker().State(), "open") {
		t.Fatal("breaker state")
	}
}

func TestTimeoutDoesNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, completion("late", 0))
	}))
	defer srv.Close()
	c := newClient(srv, "meta-llama/Llama-3.3-70B-Instruct-Turbo", "", 300)
	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.Answer(ctx, []ai.ChatMessage{{Role: "user", Content: "hi"}})
		cancel()
		if !errors.Is(err, ai.ErrTimeout) {
			t.Fatalf("want ErrTimeout, got %v", err)
		}
	}
	if c.Breaker().State() != "closed" {
		t.Fatal("caller deadlines must not open the breaker")
	}
}

func TestPromptBuildTrimsHistory(t *testing.T) {
	var history []prompt.Turn
	for i := 0; i < 20; i++ {
		history = append(history, prompt.Turn{Role: "user", Content: strings.Repeat("q", 200)},
			prompt.Turn{Role: "assistant", Content: strings.Repeat("a", 200)})
	}
	msgs := prompt.Build(prompt.Input{
		Question: "Why do they need sunlight?", History: history, MaxInputTokens: 800,
		Facts: []string{"NHIS: dial *929#."}, Now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC),
	})
	total := 0
	for _, m := range msgs {
		total += prompt.EstimateTokens(m.Content)
	}
	if total > 800 {
		t.Fatalf("prompt is %d tokens", total)
	}
	if msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "Verified Ghana Fact: NHIS") ||
		!strings.Contains(msgs[0].Content, "17 October 2026") {
		t.Fatalf("system prompt: %q", msgs[0].Content)
	}
	if msgs[1].Role != "user" || msgs[len(msgs)-1].Content != "Why do they need sunlight?" {
		t.Fatal("history must start on a user turn and end with the question")
	}
}

func TestIsReasoningModel(t *testing.T) {
	if !ai.IsReasoningModel("openai/gpt-oss-120b") || ai.IsReasoningModel("Qwen/Qwen2.5-72B-Instruct") {
		t.Fatal("IsReasoningModel")
	}
}
