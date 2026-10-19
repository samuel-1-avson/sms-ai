package service_test

// Integration tests for the plan v2.0 §16 acceptance checklist. They need
// PostgreSQL: set SMSAI_TEST_DATABASE_URL (see README "Running tests").

import (
	"context"
	"strings"
	"testing"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/channel"
	"smsaighana/internal/config"
	"smsaighana/internal/formatting"
	"smsaighana/internal/repository"
	"smsaighana/internal/testenv"
)

const phone = "0244123456"

func newEnv(t *testing.T, mutate func(*config.Config)) *testenv.Env {
	return testenv.New(t, "smsai_it_service", mutate)
}

func lastSent(t *testing.T, env *testenv.Env) string {
	t.Helper()
	bodies := env.SentBodies()
	if len(bodies) == 0 {
		t.Fatal("no SMS was sent")
	}
	return bodies[len(bodies)-1]
}

// §16 Standard QA + GSM-7 homoglyph sanitizer.
func TestStandardQuestionGetsOneGSM7Segment(t *testing.T) {
	env := newEnv(t, nil)
	res := env.SMS(phone, "What is photosynthesis?")
	if res.JobID == "" {
		t.Fatal("expected a debounced answer job")
	}
	env.Drain(5 * time.Second)

	body := lastSent(t, env)
	st := formatting.Analyze(body)
	if st.Encoding != formatting.GSM7 || st.Segments != 1 {
		t.Fatalf("reply must be one GSM-7 segment, got %+v: %q", st, body)
	}
	// The mock model answers with "Sure!", an em dash and a curly apostrophe.
	if strings.HasPrefix(body, "Sure") || strings.ContainsAny(body, "—’") {
		t.Fatalf("sanitizer did not clean the reply: %q", body)
	}
	if got := env.Sent()[0].To; got != "+233244123456" {
		t.Fatalf("sent to %q", got)
	}
}

// §16 Context continuity: the follow-up carries the earlier turn.
func TestFollowUpUsesContext(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "What is photosynthesis?")
	env.Drain(5 * time.Second)
	env.SMS(phone, "Why do they need sunlight?")
	env.Drain(5 * time.Second)

	if got := lastSent(t, env); !strings.HasPrefix(got, "Sunlight supplies the energy") {
		t.Fatalf("follow-up answer: %q", got)
	}
	req := env.MockModel.LastRequest()
	var history []string
	for _, m := range req.Messages[1 : len(req.Messages)-1] {
		history = append(history, m.Role+": "+m.Content)
	}
	joined := strings.Join(history, " | ")
	if !strings.Contains(joined, "user: What is photosynthesis?") || !strings.Contains(joined, "assistant: Photosynthesis") {
		t.Fatalf("history not passed to the model: %s", joined)
	}
}

// §16 Inbound multipart coalescing: one model call, one reply.
func TestSplitMessagesCoalesceIntoOneCall(t *testing.T) {
	env := newEnv(t, nil)
	first := env.SMS(phone, "Solve 2x + 5 = 15")
	time.Sleep(100 * time.Millisecond)
	second := env.SMS(phone, "and explain each step")
	if !second.Coalesced || second.JobID != first.JobID {
		t.Fatalf("second text should join the pending job: %+v vs %+v", second, first)
	}
	env.Drain(5 * time.Second)

	if calls := env.MockModel.Calls(); calls != 1 {
		t.Fatalf("want 1 model call, got %d", calls)
	}
	if n := len(env.Sent()); n != 1 {
		t.Fatalf("want 1 reply, got %d", n)
	}
	msgs := env.MockModel.LastRequest().Messages
	if q := msgs[len(msgs)-1].Content; q != "Solve 2x + 5 = 15 and explain each step" {
		t.Fatalf("coalesced prompt: %q", q)
	}
}

// §16 Duplicate webhooks create only one job.
func TestDuplicateWebhookIsIgnored(t *testing.T) {
	env := newEnv(t, nil)
	a := env.SMSWithID("provider-msg-1", phone, "What is photosynthesis?")
	b := env.SMSWithID("provider-msg-1", phone, "What is photosynthesis?")
	if a.Duplicate || !b.Duplicate {
		t.Fatalf("duplicate detection: %+v %+v", a, b)
	}
	env.Drain(5 * time.Second)
	if n := env.Count(`SELECT count(*) FROM jobs WHERE kind = 'answer'`); n != 1 {
		t.Fatalf("want 1 answer job, got %d", n)
	}
	if len(env.Sent()) != 1 {
		t.Fatalf("want 1 reply, got %d", len(env.Sent()))
	}
}

// §16 Stateful MORE buffer: part 2 comes from PostgreSQL with zero LLM calls.
func TestMoreServedFromBufferWithoutLLM(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "Give me a long explanation of rainfall")
	env.Drain(5 * time.Second)
	part1 := lastSent(t, env)
	if !strings.HasSuffix(part1, "..Txt MORE") || formatting.SegmentCount(part1) != 1 {
		t.Fatalf("part 1: %q", part1)
	}
	calls := env.MockModel.Calls()

	start := time.Now()
	env.SMS(phone, "MORE")
	env.Drain(5 * time.Second)
	if env.MockModel.Calls() != calls {
		t.Fatal("MORE must not call the model")
	}
	part2 := lastSent(t, env)
	if part2 == part1 || formatting.SegmentCount(part2) != 1 {
		t.Fatalf("part 2: %q", part2)
	}
	t.Logf("MORE round trip (store + dispatch) took %v", time.Since(start))

	env.SMS(phone, "MORE")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); !strings.HasPrefix(got, "Nothing more") {
		t.Fatalf("exhausted buffer: %q", got)
	}
}

// §16 Local Ghana Grounding Store: deterministic replies, zero tokens.
func TestGroundingAnswersWithoutLLM(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "How do I check my NHIS renewal?")
	env.SMS("0201234567", "How to link Ghana Card to MTN?")
	env.Drain(5 * time.Second)
	if env.MockModel.Calls() != 0 {
		t.Fatalf("grounded questions must not call the model (%d calls)", env.MockModel.Calls())
	}
	bodies := strings.Join(env.SentBodies(), "\n")
	if !strings.Contains(bodies, "*929#") || !strings.Contains(bodies, "Ghana Card to your SIM") {
		t.Fatalf("grounded replies: %s", bodies)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE answer_source = 'grounding'`); n != 2 {
		t.Fatalf("answer_source=grounding rows: %d", n)
	}
}

// §16 Reasoning-token guard: an empty reasoning answer falls back, and the
// empty call is still recorded.
func TestEmptyReasoningAnswerFallsBack(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.LLMModel, c.LLMFallbackModel = "openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo"
	})
	env.MockModel.Respond = func(req ai.ChatRequest) (string, error) {
		if req.PreferredModel == "openai/gpt-oss-20b" {
			if req.ReasoningEffort != "low" || req.MaxOutputTokens < ai.MinReasoningMaxTokens {
				t.Errorf("reasoning guard missing: %+v", req)
			}
			return "", ai.ErrEmptyResponse
		}
		return "Fallback answer from the instruct model.", nil
	}
	env.SMS(phone, "What is a noun?")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); got != "Fallback answer from the instruct model." {
		t.Fatalf("reply: %q", got)
	}
	if n := env.Count(`SELECT count(*) FROM model_calls WHERE status = 'empty'`); n != 1 {
		t.Fatalf("empty call not recorded (%d)", n)
	}
}

// §16 Sponsored voucher redemption after the daily cap.
func TestDailyLimitThenVoucher(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.DailyPerNumberFreeCap = 2 })
	for _, q := range []string{"What is a verb?", "What is an adverb?"} {
		env.SMS(phone, q)
		env.Drain(5 * time.Second)
	}
	over := env.SMS(phone, "What is a pronoun?")
	if over.Dropped != "over_limit" {
		t.Fatalf("third question should be over the limit: %+v", over)
	}
	env.SMS(phone, "What is a conjunction?")
	env.Drain(5 * time.Second)
	notices := 0
	for _, b := range env.SentBodies() {
		if strings.HasPrefix(b, "Daily limit reached (2/2)") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("want exactly one limit notice, got %d: %v", notices, env.SentBodies())
	}

	ctx := context.Background()
	if _, err := env.Store.Q().CreatePool(ctx, repository.Pool{VoucherCode: "WASSCE2026", SponsorName: "Test NGO", MaxSegments: 50, PerUserGrant: 5}); err != nil {
		t.Fatal(err)
	}
	env.SMS(phone, "CODE WASSCE2026")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); !strings.HasPrefix(got, "Code accepted: 5 extra questions") {
		t.Fatalf("voucher reply: %q", got)
	}
	env.SMS(phone, "CODE WASSCE2026")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); !strings.Contains(got, "already used") {
		t.Fatalf("second redemption: %q", got)
	}

	res := env.SMS(phone, "What is a pronoun?")
	if res.JobID == "" {
		t.Fatalf("bonus credit should unlock questions: %+v", res)
	}
	env.Drain(5 * time.Second)
	if n := env.Count(`SELECT bonus_sms_quota FROM users`); n != 4 {
		t.Fatalf("bonus balance should be 4, got %d", n)
	}
}

// §16 USSD fast path: answers render inline at zero SMS cost.
func TestUSSDInlineAnswer(t *testing.T) {
	env := newEnv(t, nil)
	if r := env.USSD("s1", phone, "", true); r.End || !strings.Contains(r.Message, "Type your question") {
		t.Fatalf("welcome: %+v", r)
	}
	r := env.USSD("s1", phone, "What is photosynthesis?", false)
	if r.End || !strings.Contains(r.Message, "Photosynthesis") || !strings.Contains(r.Message, "2.Save to SMS") {
		t.Fatalf("inline answer: %+v", r)
	}
	if len(r.Message) > 182 {
		t.Fatalf("USSD screen is %d chars", len(r.Message))
	}
	env.Drain(5 * time.Second)
	if len(env.Sent()) != 0 {
		t.Fatalf("inline answers must not send SMS: %v", env.SentBodies())
	}

	// "2" saves the answer to SMS.
	if r := env.USSD("s1", phone, "2", false); !r.End || !strings.HasPrefix(r.Message, "Sent!") {
		t.Fatalf("save to SMS: %+v", r)
	}
	env.Drain(5 * time.Second)
	if len(env.Sent()) != 1 || !strings.Contains(lastSent(t, env), "Photosynthesis") {
		t.Fatalf("saved SMS: %v", env.SentBodies())
	}
}

// §16 USSD slow fallback: close the session cleanly, deliver by MT SMS.
func TestUSSDSlowAnswerFallsBackToSMS(t *testing.T) {
	env := newEnv(t, nil)
	env.USSD("s2", phone, "", true)
	start := time.Now()
	r := env.USSD("s2", phone, "a slow question about volcanoes", false)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("USSD reply took %v; must return at the fast-path deadline", elapsed)
	}
	if !r.End || !strings.HasPrefix(r.Message, "Thinking...") {
		t.Fatalf("fallback reply: %+v", r)
	}
	env.Drain(8 * time.Second)
	if len(env.Sent()) != 1 {
		t.Fatalf("expected one MT SMS, got %v", env.SentBodies())
	}
	if n := env.Count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.channel_type = 'ussd' AND m.kind = 'answer' AND m.delivery_channel = 'sms'`); n != 1 {
		t.Fatalf("fallback answer row missing (%d)", n)
	}
	// One question, one credit, even though the model was called twice.
	if n := env.Count(`SELECT questions_used FROM usage_daily`); n != 1 {
		t.Fatalf("questions_used = %d", n)
	}
}

// §16 Key rotation: v1 users are still found after V2 becomes current, and
// are re-keyed lazily.
func TestKeyRotation(t *testing.T) {
	dbURL := testenv.FreshDatabase(t, "smsai_it_rotation")
	v1 := [2]string{testenv.TestEncryptionKey, testenv.TestHMACKey}
	v2 := [2]string{testenv.RandomKey(100), testenv.RandomKey(200)}

	envV1 := testenv.NewOn(t, dbURL, testenv.KeyEnviron(testenv.BaseEnviron(dbURL), 1, map[int][2]string{1: v1}), nil)
	envV1.SMS(phone, "What is photosynthesis?")
	envV1.Drain(5 * time.Second)
	var userID string
	if err := envV1.Store.Pool.QueryRow(context.Background(), `SELECT id::text FROM users`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	envV1.Close()

	envV2 := testenv.NewOn(t, dbURL, testenv.KeyEnviron(testenv.BaseEnviron(dbURL), 2, map[int][2]string{1: v1, 2: v2}), nil)
	envV2.SMS(phone, "Why do they need sunlight?")
	envV2.Drain(5 * time.Second)
	if n := envV2.Count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("rotation created a duplicate user (%d users)", n)
	}
	if n := envV2.Count(`SELECT count(*) FROM users WHERE id = $1 AND key_version = 2`, userID); n != 1 {
		t.Fatal("user should be re-keyed to v2")
	}
	// v1-encrypted history was still readable for context.
	if got := lastSent(t, envV2); !strings.HasPrefix(got, "Sunlight supplies") {
		t.Fatalf("context across rotation: %q", got)
	}
	st, err := envV2.Store.Rekey(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if st.Messages == 0 || envV2.Count(`SELECT count(*) FROM messages WHERE key_version <> 2`) != 0 {
		t.Fatalf("rekey left old rows: %+v", st)
	}
}

func TestStopAndStart(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "STOP")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); !strings.Contains(got, "no more messages") {
		t.Fatalf("stop reply: %q", got)
	}
	if res := env.SMS(phone, "What is photosynthesis?"); res.Dropped != "opted_out" {
		t.Fatalf("opted-out user should be ignored: %+v", res)
	}
	env.SMS(phone, "START")
	env.Drain(5 * time.Second)
	if got := lastSent(t, env); !strings.HasPrefix(got, "Welcome") {
		t.Fatalf("start reply: %q", got)
	}
	if res := env.SMS(phone, "What is photosynthesis?"); res.JobID == "" {
		t.Fatal("questions should work again after START")
	}
}

// Uncertain submissions are reconciled, never blindly resent (plan §5.4).
func TestUncertainSendIsReconciledNotResent(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()

	// Case 1: the provider accepted the message but the HTTP reply was lost.
	env.MockGW.FailNext(channel.SendUncertain)
	env.SMS(phone, "HELP")
	env.Drain(5 * time.Second)
	var msgID string
	if err := env.Store.Pool.QueryRow(ctx, `SELECT id::text FROM messages WHERE direction = 'outbound' AND status = 'unknown_timeout'`).Scan(&msgID); err != nil {
		t.Fatalf("expected an unknown_timeout message: %v", err)
	}
	env.MockGW.RecordAcceptedWithoutReply(msgID)
	env.Engine.ReconcileUncertainSends(ctx)
	env.Drain(5 * time.Second)
	if n := env.Count(`SELECT count(*) FROM messages WHERE id = $1 AND status = 'submitted'`, msgID); n != 1 {
		t.Fatal("reconciled message should be submitted")
	}
	if len(env.Sent()) != 0 {
		t.Fatal("a message the provider already has must not be sent again")
	}

	// Case 2: the provider never saw it: re-queue and send once.
	env.MockGW.FailNext(channel.SendUncertain)
	env.SMS(phone, "NEW")
	env.Drain(5 * time.Second)
	env.Engine.ReconcileUncertainSends(ctx)
	env.Drain(5 * time.Second)
	if len(env.Sent()) != 1 || env.Sent()[0].Body != "New chat started. Ask me anything." {
		t.Fatalf("re-queued message: %v", env.SentBodies())
	}
}

func TestDeliveryReceiptsAreMonotonic(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "HELP")
	env.Drain(5 * time.Second)
	id := env.Sent()[0].ProviderMessageID
	ctx := context.Background()
	for _, st := range []string{"delivered", "submitted", "delivered"} {
		if err := env.Engine.HandleDeliveryReceipts(ctx, []channel.DeliveryReceipt{{Provider: "mock", ProviderMessageID: id, Status: st}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_message_id = $1 AND status = 'delivered'`, id); n != 1 {
		t.Fatal("late 'submitted' receipt must not overwrite 'delivered'")
	}
}

func TestGlobalSegmentCapBlocksSends(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.GlobalDailySMSSegmentCap = 1 })
	env.SMS(phone, "HELP")
	env.SMS("0201234567", "HELP")
	env.Drain(5 * time.Second)
	if len(env.Sent()) != 1 {
		t.Fatalf("cap of 1 segment allows one SMS, sent %d", len(env.Sent()))
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE status = 'blocked_cap'`); n != 1 {
		t.Fatalf("blocked_cap rows: %d", n)
	}
}

func TestAIKillSwitchSendsOneBusyNoticeAndRefunds(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	if err := env.Store.Q().SetFlag(ctx, repository.FlagAIEnabled, false); err != nil {
		t.Fatal(err)
	}
	env.Engine.InvalidateFlags()
	env.SMS(phone, "What is a verb?")
	env.Drain(5 * time.Second)
	env.SMS(phone, "What is a noun?")
	env.Drain(5 * time.Second)
	if env.MockModel.Calls() != 0 {
		t.Fatal("kill switch must stop model calls")
	}
	if bodies := env.SentBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "busy") {
		t.Fatalf("want one busy notice, got %v", bodies)
	}
	if n := env.Count(`SELECT questions_used FROM usage_daily`); n != 0 {
		t.Fatalf("unanswered questions must be refunded, questions_used=%d", n)
	}
	// Grounded questions still work with the AI switched off.
	env.SMS(phone, "How do I check my NHIS renewal?")
	env.Drain(5 * time.Second)
	if !strings.Contains(lastSent(t, env), "*929#") {
		t.Fatal("grounding should keep working when AI is off")
	}
}

func TestDeleteRemovesHistory(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "What is photosynthesis?")
	env.Drain(5 * time.Second)
	env.SMS(phone, "DELETE")
	env.Drain(5 * time.Second)
	if n := env.Count(`SELECT count(*) FROM messages WHERE kind IN ('question', 'answer')`); n != 0 {
		t.Fatalf("history should be deleted, %d rows left", n)
	}
	if got := lastSent(t, env); !strings.Contains(got, "deleted") {
		t.Fatalf("delete reply: %q", got)
	}
}

func TestPIIIsScrubbedBeforeStorageAndModel(t *testing.T) {
	env := newEnv(t, nil)
	env.SMS(phone, "My MoMo PIN is 4321 and my card GHA-123456789-0 is not working, help")
	env.Drain(5 * time.Second)
	msgs := env.MockModel.LastRequest().Messages
	q := msgs[len(msgs)-1].Content
	if strings.Contains(q, "4321") || strings.Contains(q, "GHA-123456789-0") {
		t.Fatalf("PII reached the model: %q", q)
	}
	turns, err := env.Store.Q().RecentTurns(context.Background(), firstConversation(t, env), "", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Content, "4321") {
			t.Fatalf("PII stored: %q", turn.Content)
		}
	}
}

func firstConversation(t *testing.T, env *testenv.Env) string {
	t.Helper()
	var id string
	if err := env.Store.Pool.QueryRow(context.Background(), `SELECT id::text FROM conversations LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
