// Command aibench benchmarks candidate DeepInfra models for the SMS use case
// (plan §7.3 and §16 "Reasoning-Token Guard"): empty-response rate, latency
// against the USSD fast-path deadline, GSM-7 single-segment fit after
// sanitizing, and estimated cost.
//
//	DEEPINFRA_API_KEY=... aibench -n 100 -models openai/gpt-oss-20b,meta-llama/Llama-3.3-70B-Instruct-Turbo
//
// With LLM_PROVIDER=mock it runs against the mock model (no key needed).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"smsaighana/internal/ai"
	"smsaighana/internal/ai/deepinfra"
	mockai "smsaighana/internal/ai/mock"
	"smsaighana/internal/ai/prompt"
	"smsaighana/internal/config"
	"smsaighana/internal/formatting"
)

type result struct {
	latency   time.Duration
	err       error
	empty     bool
	rawUCS2   bool
	oneSeg    bool
	chars     int
	reasoning int
	cost      float64
	prompt    string
	answer    string // sanitized
	nonGSM    []rune // characters in the raw answer the sanitizer had to fix
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	models := flag.String("models", cfg.LLMModel+","+cfg.LLMFallbackModel, "comma-separated models to compare")
	promptsFile := flag.String("prompts", "tests/ai_eval/prompts.txt", "prompt file, one per line")
	n := flag.Int("n", 100, "calls per model (prompts are cycled)")
	concurrency := flag.Int("concurrency", 4, "parallel requests per model")
	timeout := flag.Duration("timeout", 15*time.Second, "per-call timeout")
	fastPath := flag.Duration("fast-path", cfg.USSDFastPathTimeout, "USSD inline deadline to score against")
	verbose := flag.Bool("v", false, "print answers that need more than one SMS and the non-GSM characters models emit")
	flag.Parse()

	prompts, err := readPrompts(*promptsFile)
	if err != nil {
		fail(err)
	}
	var model ai.ChatModel
	switch {
	case cfg.LLMProvider == "mock":
		model = mockai.New(cfg.MockLLMLatency)
	case cfg.DeepInfraAPIKey == "":
		fail(errors.New("DEEPINFRA_API_KEY is not set (or set LLM_PROVIDER=mock for a dry run)"))
	default:
		model = deepinfra.New(cfg.DeepInfraBaseURL, cfg.DeepInfraAPIKey)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "model\tcalls\terrors\tempty\tp50\tp95\tmax\t<=fast-path\t1-seg\tneeds MORE\traw UCS-2\tavg chars\tavg reasoning tok\tUSD/1k q\t")
	for _, m := range strings.Split(*models, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		// No fallback: each model is measured on its own, with the same
		// reasoning guard the service applies.
		client := ai.NewClient(model, ai.ClientOptions{
			Primary: m, ReasoningEffort: cfg.LLMReasoningEffort, MaxOutputTokens: cfg.LLMMaxOutputTokens,
			Temperature: cfg.LLMTemperature, Prices: cfg.LLMPrices, Breaker: ai.NewBreaker(1<<30, 0, 0),
		})
		fmt.Fprintf(os.Stderr, "benchmarking %s (%d calls)...\n", m, *n)
		results := run(client, prompts, *n, *concurrency, *timeout)
		writeRow(w, m, results, *fastPath)
		if *verbose {
			printDetails(m, results)
		}
	}
	w.Flush()
	fmt.Println("\n1-seg = answer fits one GSM-7 SMS after sanitizing; raw UCS-2 = model output had characters the sanitizer had to fix.")
	fmt.Println("Exit criterion (plan §15 week 1): empty must be 0 for reasoning models with reasoning_effort=low.")
}

func run(client *ai.Client, prompts []string, n, concurrency int, timeout time.Duration) []result {
	results := make([]result, n)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			msgs := prompt.Build(prompt.Input{Question: prompts[i%len(prompts)], Now: time.Now(), MaxInputTokens: 800})
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			start := time.Now()
			out, err := client.Answer(ctx, msgs)
			cancel()
			r := result{latency: time.Since(start), err: err, prompt: prompts[i%len(prompts)]}
			if len(out.Calls) > 0 {
				c := out.Calls[0]
				r.cost, r.reasoning = c.CostUSD, c.ReasoningTokens
				r.empty = c.Status == "empty"
			}
			if err == nil {
				clean := formatting.Sanitize(out.Text, formatting.SanitizeOptions{})
				r.rawUCS2 = !formatting.IsGSM7Text(out.Text)
				r.chars = len(clean)
				r.oneSeg = formatting.Fits(clean, formatting.SMSLimits)
				r.answer = clean
				for _, ch := range out.Text {
					if !formatting.IsGSM7(ch) {
						r.nonGSM = append(r.nonGSM, ch)
					}
				}
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	return results
}

func writeRow(w *tabwriter.Writer, model string, rs []result, fastPath time.Duration) {
	var lat []time.Duration
	var errs, empty, fast, oneSeg, ok, ucs2, chars, reasoning int
	var cost float64
	for _, r := range rs {
		lat = append(lat, r.latency)
		cost += r.cost
		reasoning += r.reasoning
		if r.empty {
			empty++
		}
		if r.err != nil {
			if !r.empty {
				errs++
			}
			continue
		}
		ok++
		chars += r.chars
		if r.latency <= fastPath {
			fast++
		}
		if r.oneSeg {
			oneSeg++
		}
		if r.rawUCS2 {
			ucs2++
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(b))
	}
	avg := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", a/b)
	}
	fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%v\t%v\t%v\t%s\t%s\t%s\t%s\t%s\t%s\t$%.4f\t\n",
		model, len(rs), errs, empty, quantile(lat, 0.5), quantile(lat, 0.95), quantile(lat, 1),
		pct(fast, len(rs)), pct(oneSeg, ok), pct(ok-oneSeg, ok), pct(ucs2, ok), avg(chars, ok),
		avg(reasoning, len(rs)), cost/float64(len(rs))*1000)
}

// printDetails lists answers that overflow one SMS (to tune the prompt) and
// the non-GSM characters seen in raw output (to extend the sanitizer).
func printDetails(model string, rs []result) {
	fmt.Fprintf(os.Stderr, "\n--- %s: answers longer than one SMS ---\n", model)
	chars := map[rune]int{}
	for _, r := range rs {
		for _, ch := range r.nonGSM {
			chars[ch]++
		}
		if r.err == nil && !r.oneSeg {
			fmt.Fprintf(os.Stderr, "[%d chars] %s\n    -> %s\n", r.chars, r.prompt, r.answer)
		}
	}
	type kv struct {
		ch rune
		n  int
	}
	var list []kv
	for ch, n := range chars {
		list = append(list, kv{ch, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	fmt.Fprintf(os.Stderr, "--- %s: non-GSM characters in raw output ---\n", model)
	for _, e := range list {
		fmt.Fprintf(os.Stderr, "%q U+%04X x%d\n", e.ch, e.ch, e.n)
	}
}

func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i].Round(10 * time.Millisecond)
}

func readPrompts(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no prompts", path)
	}
	return out, sc.Err()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "aibench:", err)
	os.Exit(1)
}
