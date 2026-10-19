// Package prompt assembles the model input: the hardened v2.0 system prompt
// (plan §7.4), verified grounding facts, a bounded window of recent turns
// and the user's question.
package prompt

import (
	"strings"
	"time"

	"smsaighana/internal/ai"
)

// SystemPrompt is the plan v2.0 §7.4 prompt, tightened after the
// 2026-10-17 benchmark: the reply must normally fit one 160-character SMS,
// and only step-by-step answers may run longer (served via MORE).
const SystemPrompt = `You are SMS AI Ghana, a concise, accurate text assistant for users in Ghana.
Every reply is sent as ONE text message of at most 150 characters (about 25 words). Count carefully.
Give the direct answer in one or two short sentences. Leave out greetings, background and examples unless asked.
Only when the user asks for steps or a worked solution may you go longer, up to 300 characters, still with the key result first.
Write plain ASCII only: straight quotes ' and ", hyphen -, x for times, / for divide, ^ for powers, GHS for cedis. No other symbols.
Never use markdown, bold, lists, line breaks, or emojis.
Use conversation history only to resolve follow-up pronouns or context.
Do not invent live prices, flight schedules, news, exchange rates, or bookings; state clearly when live info is unavailable.
Do not name specific companies, agencies, websites or phone numbers unless they are official and you are sure they exist; give general guidance instead.
For health, legal, or financial topics, give general educational facts and advise consulting a qualified professional.
Decline harmful or illegal requests briefly. Never ask for PINs, MoMo OTPs, passwords, or Ghana Card numbers.`

type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

type Input struct {
	Question       string
	History        []Turn // oldest first
	Facts          []string
	Now            time.Time
	MaxInputTokens int
}

// EstimateTokens is a conservative chars/4 approximation; exact counts come
// back from the provider and are recorded in model_calls.
func EstimateTokens(s string) int { return (len(s)+3)/4 + 4 }

// Build returns the chat messages, dropping the oldest history turns until
// the estimated input fits MaxInputTokens.
func Build(in Input) []ai.ChatMessage {
	var sys strings.Builder
	sys.WriteString(SystemPrompt)
	if !in.Now.IsZero() {
		sys.WriteString("\nToday's date is ")
		sys.WriteString(in.Now.UTC().Format("2 January 2006"))
		sys.WriteString(".")
	}
	for _, f := range in.Facts {
		sys.WriteString("\nVerified Ghana Fact: ")
		sys.WriteString(strings.TrimSpace(f))
	}
	system := ai.ChatMessage{Role: "system", Content: sys.String()}
	question := ai.ChatMessage{Role: "user", Content: in.Question}

	budget := in.MaxInputTokens
	if budget <= 0 {
		budget = 800
	}
	used := EstimateTokens(system.Content) + EstimateTokens(question.Content)

	history := in.History
	total := used
	for _, t := range history {
		total += EstimateTokens(t.Content)
	}
	for len(history) > 0 && total > budget {
		total -= EstimateTokens(history[0].Content)
		history = history[1:]
	}
	// Never start the window on an assistant turn without its question.
	for len(history) > 0 && history[0].Role != "user" {
		history = history[1:]
	}

	msgs := make([]ai.ChatMessage, 0, len(history)+2)
	msgs = append(msgs, system)
	for _, t := range history {
		msgs = append(msgs, ai.ChatMessage{Role: t.Role, Content: t.Content})
	}
	return append(msgs, question)
}
