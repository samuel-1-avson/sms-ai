// Command load drives a running stack with signed mock inbound SMS webhooks
// at a fixed rate from many distinct phone numbers (plan §15 week 6). It
// reports webhook ack latency percentiles and end-to-end throughput: answer
// jobs finished per minute, read from GET /admin/usage while the workers
// drain the queue. See tests/load/README.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mockch "smsaighana/internal/channel/mock"
)

// Questions avoid the mock model's trigger words (slow, long, empty, fail).
var questions = []string{
	"What is photosynthesis?",
	"What causes malaria and how can I prevent it?",
	"How do I prepare for the BECE maths paper?",
	"When is the best time to plant maize in the north?",
	"What does a balanced diet for a child contain?",
	"How do I calculate simple interest on a loan?",
}

// Mobile prefixes across MTN, Telecel and AT so carrier metrics are mixed.
var prefixes = []string{"24", "54", "55", "59", "20", "50", "26", "27", "56", "57"}

type options struct {
	target, secret, adminToken, serviceID string
	rate                                  float64
	duration, drainTimeout, maxP95        time.Duration
	numbers, maxInflight                  int
	allowRealLLM                          bool
}

func main() {
	var o options
	flag.StringVar(&o.target, "target", envOr("LOAD_TARGET", "http://localhost:8088"), "API base URL")
	flag.Float64Var(&o.rate, "rate", 20, "webhooks per second (open loop)")
	flag.DurationVar(&o.duration, "duration", time.Minute, "how long to send")
	flag.IntVar(&o.numbers, "numbers", 5000, "distinct phone numbers, 1-100000; texts cycle through them")
	flag.IntVar(&o.maxInflight, "max-inflight", 512, "cap on concurrent webhook requests")
	flag.DurationVar(&o.drainTimeout, "drain-timeout", 5*time.Minute, "wait this long for the queue to drain (0 skips)")
	flag.DurationVar(&o.maxP95, "max-p95", 500*time.Millisecond, "exit 1 when the ack p95 is above this")
	flag.StringVar(&o.secret, "secret", envOr("MOCK_WEBHOOK_SECRET", "dev-mock-webhook-secret"), "mock webhook HMAC secret")
	flag.StringVar(&o.adminToken, "admin-token", os.Getenv("ADMIN_API_TOKEN"), "bearer token for /admin (blank in open development)")
	flag.StringVar(&o.serviceID, "to", envOr("SMS_SERVICE_ID", "SIM-SHORTCODE"), "short code the texts are sent to")
	flag.BoolVar(&o.allowRealLLM, "allow-real-llm", false, "run even when the stack uses a paid LLM provider")
	flag.Parse()
	if o.rate <= 0 || o.duration <= 0 || o.numbers < 1 || o.numbers > 100000 || o.maxInflight < 1 {
		fmt.Fprintln(os.Stderr, "load: need rate > 0, duration > 0, 1 <= numbers <= 100000, max-inflight >= 1")
		os.Exit(2)
	}
	o.target = strings.TrimRight(o.target, "/")
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns: o.maxInflight, MaxIdleConnsPerHost: o.maxInflight, IdleConnTimeout: 30 * time.Second,
		},
	}
	admin := adminClient{client: client, base: o.target, token: o.adminToken}

	var status struct {
		Env         string `json:"env"`
		LLMProvider string `json:"llm_provider"`
	}
	if err := admin.get("/admin/status", &status); err != nil {
		return fmt.Errorf("preflight: %w (is the stack up? see tests/load/README.md)", err)
	}
	if status.Env == "production" {
		return errors.New("refusing to load-test a production stack")
	}
	if status.LLMProvider != "mock" && !o.allowRealLLM {
		return fmt.Errorf("the stack uses LLM provider %q; every webhook would be a paid model call (pass -allow-real-llm to go ahead)", status.LLMProvider)
	}
	before, err := admin.usage()
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	total := int(o.rate * o.duration.Seconds())
	run := randomRun()
	fmt.Printf("load: %d webhooks at %.1f/s over %v from %d numbers (run %02d, env %s, llm %s) -> %s\n",
		total, o.rate, o.duration, min(o.numbers, total), run, status.Env, status.LLMProvider, o.target)

	// Answers are sampled from /admin/usage throughout, so throughput covers
	// the sending phase as well as the drain.
	start := time.Now()
	sampler := newSampler(admin, before)
	sctx, stopSampling := context.WithCancel(context.Background())
	samplerDone := make(chan struct{})
	go func() { sampler.run(sctx, 2*time.Second); close(samplerDone) }()

	res := send(o, client, run, total, start)
	sendEnd := time.Now()

	drained, drainedAt := false, time.Time{}
	if o.drainTimeout > 0 {
		drained, drainedAt = sampler.waitDrained(sendEnd.Add(o.drainTimeout), before.open())
	}
	stopSampling()
	<-samplerDone
	after, err := admin.usage()
	if err != nil {
		return err
	}

	return report(o, res, sampler, before, after, start, sendEnd, drained, drainedAt)
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

type sendResult struct {
	latency  []time.Duration // from scheduled time to response; -1 when no response
	status   []int
	errs     map[string]int
	maxLag   time.Duration // how far the generator fell behind its schedule
	inflight atomic.Int64
}

func phone(run, i int) string {
	return fmt.Sprintf("+233%s%02d%05d", prefixes[i%len(prefixes)], run, i)
}

// send fires total webhooks on a fixed schedule. Latency is measured from
// each request's scheduled time, so a slow server cannot hide queueing
// behind a slowed-down generator.
func send(o options, client *http.Client, run, total int, start time.Time) *sendResult {
	res := &sendResult{latency: make([]time.Duration, total), status: make([]int, total), errs: map[string]int{}}
	var errMu sync.Mutex
	sem := make(chan struct{}, o.maxInflight)
	interval := time.Duration(float64(time.Second) / o.rate)
	url := o.target + "/webhooks/sms/mock/inbound"
	var wg sync.WaitGroup

	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
	acked := atomic.Int64{}

	for i := 0; i < total; i++ {
		due := start.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		select {
		case <-progress.C:
			fmt.Printf("  %5.0fs sent %d, acked %d, in flight %d\n", time.Since(start).Seconds(), i, acked.Load(), res.inflight.Load())
		default:
		}
		sem <- struct{}{}
		if lag := time.Since(due); lag > res.maxLag {
			res.maxLag = lag
		}
		wg.Add(1)
		res.inflight.Add(1)
		go func(i int, due time.Time) {
			defer func() { <-sem; res.inflight.Add(-1); wg.Done() }()
			body, _ := json.Marshal(mockch.InboundPayload{
				MessageID: fmt.Sprintf("load-%02d-%d-%d", run, start.Unix(), i),
				From:      phone(run, i%o.numbers), To: o.serviceID, Text: questions[i%len(questions)],
			})
			code, err := post(client, o.secret, url, body)
			res.latency[i], res.status[i] = time.Since(due), code
			if err != nil {
				res.latency[i] = -1
				errMu.Lock()
				res.errs[shortErr(err)]++
				errMu.Unlock()
				return
			}
			acked.Add(1)
		}(i, due)
	}
	wg.Wait()
	return res
}

func post(client *http.Client, secret, url string, body []byte) (int, error) {
	req, err := mockch.SignedRequest(context.Background(), secret, url, body, time.Now())
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && !strings.HasPrefix(s, "HTTP") {
		s = s[i+2:]
	}
	return s
}

// ---------------------------------------------------------------------------
// Admin API
// ---------------------------------------------------------------------------

type adminClient struct {
	client      *http.Client
	base, token string
}

func (a adminClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, a.base+path, nil)
	if err != nil {
		return err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: HTTP %d: %s (set ADMIN_API_TOKEN if the stack has one)", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// usage is the part of /admin/usage the load test reads. Counts are for
// the current UTC day, so runs compare snapshots taken before and after.
type usage struct {
	Questions         int            `json:"questions"`
	QuestionsByResult map[string]int `json:"questions_by_result"`
	CoalescedMessages int            `json:"coalesced_messages"`
	OutboundByStatus  map[string]int `json:"outbound_by_status"`
	AICalls           int            `json:"ai_calls"`
	AIAvgLatencyMS    float64        `json:"ai_avg_latency_ms"`
	UsersNew          int            `json:"users_new"`
}

func (a adminClient) usage() (usage, error) {
	var u usage
	err := a.get("/admin/usage", &u)
	return u, err
}

// open counts answer jobs still pending or running.
func (u usage) open() int { return u.QuestionsByResult["pending"] + u.QuestionsByResult["running"] }

// answered counts finished answer jobs that produced a reply; submitted
// narrows that to replies the SMS gateway accepted (the job result gets a
// suffix such as _blocked_cap otherwise).
func (u usage) answered() (answered, submitted int) {
	for k, n := range u.QuestionsByResult {
		if !strings.HasPrefix(k, "answered") {
			continue
		}
		answered += n
		if !strings.Contains(k, "blocked") && !strings.Contains(k, "unknown") && !strings.Contains(k, "failed") {
			submitted += n
		}
	}
	return answered, submitted
}

// ---------------------------------------------------------------------------
// Throughput sampling
// ---------------------------------------------------------------------------

type sample struct {
	at        time.Time
	submitted int // delta since the run started
	open      int
}

type sampler struct {
	admin    adminClient
	baseline int
	mu       sync.Mutex
	samples  []sample
	errs     int
}

func newSampler(admin adminClient, before usage) *sampler {
	_, sub := before.answered()
	return &sampler{admin: admin, baseline: sub, samples: []sample{{at: time.Now(), open: before.open()}}}
}

func (s *sampler) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u, err := s.admin.usage()
			s.mu.Lock()
			if err != nil {
				s.errs++
			} else {
				_, sub := u.answered()
				s.samples = append(s.samples, sample{at: time.Now(), submitted: sub - s.baseline, open: u.open()})
			}
			s.mu.Unlock()
		}
	}
}

// waitDrained blocks until no more answer jobs are open than before the
// run, or until deadline.
func (s *sampler) waitDrained(deadline time.Time, openBefore int) (bool, time.Time) {
	fmt.Println("  sending done; waiting for the workers to drain the queue")
	lastPrint := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		s.mu.Lock()
		n, last := len(s.samples), s.samples[len(s.samples)-1]
		s.mu.Unlock()
		if n > 1 && last.open <= openBefore {
			return true, last.at
		}
		if time.Since(lastPrint) >= 5*time.Second {
			fmt.Printf("  open answer jobs %d, answers submitted %d\n", last.open, last.submitted)
			lastPrint = time.Now()
		}
	}
	return false, time.Time{}
}

// lastAnswerAt is the first sample that saw the final answer count.
func (s *sampler) lastAnswerAt() (time.Time, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	final := s.samples[len(s.samples)-1].submitted
	for _, sm := range s.samples {
		if sm.submitted == final {
			return sm.at, final
		}
	}
	return time.Time{}, final
}

const peakWindow = 30 * time.Second

func (s *sampler) peak() string {
	if r, ok := s.peakPerMinute(peakWindow); ok {
		return fmt.Sprintf("peak %.0f/min over %v", r, peakWindow)
	}
	return fmt.Sprintf("no peak (run shorter than %v)", peakWindow)
}

// peakPerMinute is the best answers-per-minute rate over any window of at
// least span between samples.
func (s *sampler) peakPerMinute(span time.Duration) (best float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.samples {
		for j := i + 1; j < len(s.samples); j++ {
			d := s.samples[j].at.Sub(s.samples[i].at)
			if d < span {
				continue
			}
			if r := float64(s.samples[j].submitted-s.samples[i].submitted) / d.Minutes(); r > best {
				best = r
			}
			ok = true
			break
		}
	}
	return best, ok
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

func report(o options, res *sendResult, s *sampler, before, after usage, start, sendEnd time.Time, drained bool, drainedAt time.Time) error {
	var ok []time.Duration
	codes := map[int]int{}
	for i, d := range res.latency {
		if d >= 0 {
			ok = append(ok, d)
		}
		if res.status[i] != 0 && res.status[i] != http.StatusOK {
			codes[res.status[i]]++
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
	p := func(q float64) time.Duration {
		if len(ok) == 0 {
			return 0
		}
		return ok[min(len(ok)-1, int(q*float64(len(ok))))]
	}
	total := len(res.latency)
	sendFor := sendEnd.Sub(start)

	fmt.Println()
	fmt.Println("== SMS AI load test ==")
	fmt.Printf("target          %s\n", o.target)
	fmt.Printf("offered         %d webhooks at %.1f/s for %v (achieved %.1f/s), %d numbers\n",
		total, o.rate, o.duration, float64(total)/sendFor.Seconds(), min(o.numbers, total))
	fmt.Printf("acked           %d ok, %d failed", len(ok), total-len(ok))
	if len(codes) > 0 || len(res.errs) > 0 {
		fmt.Printf(" (HTTP status %v, errors %v)", codes, res.errs)
	}
	fmt.Println()
	verdict := "PASS"
	if p(0.95) > o.maxP95 || len(ok) < total {
		verdict = "FAIL"
	}
	fmt.Printf("ack latency     p50 %v  p95 %v  p99 %v  max %v  (target p95 < %v: %s)\n",
		ms(p(0.50)), ms(p(0.95)), ms(p(0.99)), ms(p(1)), o.maxP95, verdict)
	fmt.Printf("generator lag   max %v behind schedule\n", ms(res.maxLag))

	ans, sub := after.answered()
	ans0, sub0 := before.answered()
	lastAt, _ := s.lastAnswerAt()
	fmt.Printf("answer jobs     %d new questions, %d coalesced texts, %d answered (%d submitted to the gateway)\n",
		after.Questions-before.Questions, after.CoalescedMessages-before.CoalescedMessages, ans-ans0, sub-sub0)
	if drained || o.drainTimeout == 0 {
		window := lastAt.Sub(start)
		if o.drainTimeout > 0 && window > 0 {
			fmt.Printf("throughput      %.0f answers/min end to end (first webhook to last answer, %v); %s\n",
				float64(sub-sub0)/window.Minutes(), window.Round(time.Second), s.peak())
			fmt.Printf("drain           queue empty %v after the last webhook\n", drainedAt.Sub(sendEnd).Round(100*time.Millisecond))
		}
	} else {
		verdict = "FAIL"
		fmt.Printf("drain           NOT drained within %v (%d answer jobs still open); %s\n",
			o.drainTimeout, after.open(), s.peak())
	}
	fmt.Printf("model           %d calls, avg latency %.0f ms today\n", after.AICalls-before.AICalls, after.AIAvgLatencyMS)
	fmt.Printf("results         %s\n", deltaString(before.QuestionsByResult, after.QuestionsByResult))
	fmt.Printf("outbound        %s\n", deltaString(before.OutboundByStatus, after.OutboundByStatus))
	if s.errs > 0 {
		fmt.Printf("note            %d usage polls failed\n", s.errs)
	}
	if verdict == "FAIL" {
		return errors.New("targets not met")
	}
	return nil
}

func deltaString(before, after map[string]int) string {
	var keys []string
	for k := range after {
		if after[k] != before[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %+d", k, after[k]-before[k]))
	}
	if len(parts) == 0 {
		return "(no change)"
	}
	return strings.Join(parts, ", ")
}

func ms(d time.Duration) time.Duration {
	if d < 10*time.Millisecond {
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// randomRun picks which block of numbers this run texts from, so repeated
// runs on one day rarely reuse numbers (each has a daily question cap).
func randomRun() int {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int(binary.BigEndian.Uint64(b[:]) % 100)
}
