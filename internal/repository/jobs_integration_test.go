package repository_test

// Integration tests for the job queue. They need PostgreSQL: set
// SMSAI_TEST_DATABASE_URL (see README "Running tests").

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/repository"
	"smsaighana/internal/testenv"
)

// conversationWithRetryAndPending sets up the state behind the claim race:
// an answer job whose scheduled retry is due (running, lease expired,
// attempts left) and a newer matured pending answer job for the same
// conversation. offset orders jobs across conversations so concurrent
// claimers meet the pending job right after the retry they skipped.
func conversationWithRetryAndPending(t *testing.T, env *testenv.Env, phone string, offset int) (convID, retryID, pendingID string) {
	t.Helper()
	ctx := context.Background()
	q := env.Store.Q()
	user, _, err := q.FindOrCreateUser(ctx, phone, "mtn")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := q.TouchConversation(ctx, user.ID, testenv.ShortCode, "sms", "mock", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Pool.QueryRow(ctx, `INSERT INTO jobs
		(conversation_id, kind, status, attempts, max_attempts, available_at, lease_expires_at, created_at)
		VALUES ($1, 'answer', 'running', 1, 3, now() - interval '1 hour' + make_interval(secs => $2),
			now() - interval '1 second', now() - interval '1 hour')
		RETURNING id::text`, conv.ID, 2*offset).Scan(&retryID); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Pool.QueryRow(ctx, `INSERT INTO jobs (conversation_id, kind, status, max_attempts, available_at)
		VALUES ($1, 'answer', 'pending', 3, now() - interval '1 hour' + make_interval(secs => $2))
		RETURNING id::text`, conv.ID, 2*offset+1).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	return conv.ID, retryID, pendingID
}

// claimAll runs workers goroutines that call ClaimJob at the same instant
// and keep claiming until nothing is runnable. Claimed jobs are not
// finished, so every claim holds a live lease until the test releases it.
func claimAll(t *testing.T, env *testenv.Env, workers int) []*repository.Job {
	t.Helper()
	var (
		mu      sync.Mutex
		claimed []*repository.Job
		errs    []error
		wg      sync.WaitGroup
	)
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			for {
				job, err := env.Store.Q().ClaimJob(context.Background(), owner, time.Minute)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else if job != nil {
					claimed = append(claimed, job)
				}
				mu.Unlock()
				if err != nil || job == nil {
					return
				}
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("ClaimJob: %v", errs[0])
	}
	return claimed
}

// Answer jobs for one conversation must never run concurrently: while a
// due retry is being claimed, other workers must not take the newer
// pending job. Before the fix their NOT EXISTS check could not see the
// uncommitted claim and both jobs ran at once.
func TestClaimJobNeverRunsTwoAnswerJobsOfOneConversation(t *testing.T) {
	env := testenv.New(t, "smsai_it_repository", nil)
	ctx := context.Background()
	const rounds, conversations, workers = 5, 20, 8

	for round := 0; round < rounds; round++ {
		retryOf := map[string]string{}
		pendingOf := map[string]string{}
		for i := 0; i < conversations; i++ {
			conv, retry, pending := conversationWithRetryAndPending(t, env, fmt.Sprintf("+23324%07d", round*1000+i), i)
			retryOf[conv], pendingOf[conv] = retry, pending
		}

		perConv := map[string][]string{}
		for _, j := range claimAll(t, env, workers) {
			if _, ours := retryOf[j.ConversationID]; ours {
				perConv[j.ConversationID] = append(perConv[j.ConversationID], j.ID)
			}
		}
		for conv, retry := range retryOf {
			got := perConv[conv]
			if len(got) != 1 || got[0] != retry {
				t.Fatalf("round %d: conversation %s: claimed %v, want only the due retry %s (pending job %s must wait)",
					round, conv, got, retry, pendingOf[conv])
			}
		}

		// Once the retry finishes, the pending job is next.
		for _, retry := range retryOf {
			if err := env.Store.Q().CompleteJob(ctx, retry, "answered_llm"); err != nil {
				t.Fatal(err)
			}
		}
		perConv = map[string][]string{}
		for _, j := range claimAll(t, env, workers) {
			perConv[j.ConversationID] = append(perConv[j.ConversationID], j.ID)
		}
		for conv, pending := range pendingOf {
			if got := perConv[conv]; len(got) != 1 || got[0] != pending {
				t.Fatalf("round %d: after the retry finished, conversation %s claimed %v, want %s", round, conv, got, pending)
			}
			if err := env.Store.Q().CompleteJob(ctx, pending, "answered_llm"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Two due retries for one conversation (left behind by the old race or a
// lease that lapsed mid-job) must not block each other: the older runs
// first, then the newer.
func TestClaimJobRunsOlderOfTwoDueRetriesFirst(t *testing.T) {
	env := testenv.New(t, "smsai_it_repository", nil)
	ctx := context.Background()
	_, older, newer := conversationWithRetryAndPending(t, env, "+233244000002", 0)
	if _, err := env.Store.Pool.Exec(ctx, `UPDATE jobs SET status = 'running', attempts = 1,
		lease_expires_at = now() - interval '1 second', created_at = now() - interval '30 minutes' WHERE id = $1`, newer); err != nil {
		t.Fatal(err)
	}
	claim := func() string {
		t.Helper()
		job, err := env.Store.Q().ClaimJob(ctx, "w", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			return ""
		}
		return job.ID
	}
	if got := claim(); got != older {
		t.Fatalf("first claim %q, want the older retry %s", got, older)
	}
	if got := claim(); got != "" {
		t.Fatalf("claimed %s while the older retry runs", got)
	}
	if err := env.Store.Q().CompleteJob(ctx, older, "answered_llm"); err != nil {
		t.Fatal(err)
	}
	if got := claim(); got != newer {
		t.Fatalf("after the older retry finished, claimed %q, want %s", got, newer)
	}
}

// A retry that used its last attempt is never claimed again, so it must
// not hold up the conversation's next question.
func TestClaimJobSkipsPastExhaustedRetry(t *testing.T) {
	env := testenv.New(t, "smsai_it_repository", nil)
	ctx := context.Background()
	_, retry, pending := conversationWithRetryAndPending(t, env, "+233244000001", 0)
	if _, err := env.Store.Pool.Exec(ctx, `UPDATE jobs SET attempts = max_attempts WHERE id = $1`, retry); err != nil {
		t.Fatal(err)
	}
	job, err := env.Store.Q().ClaimJob(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != pending {
		t.Fatalf("claimed %+v, want the pending job %s", job, pending)
	}
}
