package worker_test

// Integration test for the worker loop. It needs PostgreSQL: set
// SMSAI_TEST_DATABASE_URL (see README "Running tests"). Run it with -race
// (compose profile test-race) to check the concurrent paths.

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/channel"
	mockch "smsaighana/internal/channel/mock"
	"smsaighana/internal/testenv"
	"smsaighana/internal/worker"
)

var askedRE = regexp.MustCompile(`q(\d) n\d+`)

// Concurrent workers answer every question once and in order per number,
// even when a failed send is retried while the next question waits behind
// it (the retry and the newer job become runnable at the same moment).
func TestWorkersAnswerInOrderAcrossRetries(t *testing.T) {
	env := testenv.New(t, "smsai_it_worker", nil)
	const numbers = 12
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.New(env.Engine, env.Store, env.Cfg, env.Log).Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("worker did not stop")
		}
	}()

	phone := func(i int) string { return fmt.Sprintf("+233245%06d", i) }
	inbound := func(q int) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, numbers)
		for i := 0; i < numbers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := env.Engine.HandleInboundSMS(context.Background(), channel.InboundSMS{
					Provider: mockch.Name, ProviderEventID: fmt.Sprintf("worker-q%d-n%d", q, i), ServiceID: testenv.ShortCode,
					FromE164: phone(i), Body: fmt.Sprintf("q%d n%02d", q, i), ReceivedAt: time.Now(),
				})
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	// Half of the first answers fail to send and are retried after the
	// backoff; the second questions arrive while those retries wait.
	env.MockGW.FailNext(repeat(channel.SendRetryable, numbers/2)...)
	inbound(1)
	waitFor(t, 5*time.Second, func() bool {
		return env.Count(`SELECT count(*) FROM jobs WHERE status = 'pending'`) == 0
	})
	inbound(2)
	waitFor(t, 30*time.Second, func() bool {
		return env.Count(`SELECT count(*) FROM jobs WHERE status = 'pending' OR (status = 'running' AND attempts < max_attempts)`) == 0
	})

	if n := env.Count(`SELECT count(*) FROM jobs WHERE status <> 'done'`); n != 0 {
		t.Fatalf("%d jobs did not finish", n)
	}
	if n := env.Count(`SELECT count(*) FROM jobs WHERE attempts > 1`); n == 0 {
		t.Fatal("no job was retried; the injected send failures did not take effect")
	}
	asked := map[string][]string{}
	for _, m := range env.Sent() {
		if q := askedRE.FindStringSubmatch(m.Body); q != nil {
			asked[m.To] = append(asked[m.To], q[1])
		}
	}
	for i := 0; i < numbers; i++ {
		if got := asked[phone(i)]; fmt.Sprint(got) != "[1 2]" {
			t.Errorf("%s: replies answered questions %v, want [1 2] in order", phone(i), got)
		}
	}
}

func repeat(k channel.SendErrorKind, n int) []channel.SendErrorKind {
	out := make([]channel.SendErrorKind, n)
	for i := range out {
		out[i] = k
	}
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
