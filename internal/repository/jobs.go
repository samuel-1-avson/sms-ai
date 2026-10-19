package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	JobAnswer = "answer"
	JobSend   = "send"
)

type Job struct {
	ID              string
	ConversationID  string
	Kind            string
	Status          string
	LatestMessageID *string
	ReplyMessageID  *string
	CoalesceCount   int
	AllowUCS2       bool
	CreditSource    *string
	CreditDay       *time.Time
	Attempts        int
	MaxAttempts     int
	CreatedAt       time.Time
}

const jobCols = `id::text, conversation_id::text, kind, status, latest_message_id::text, reply_message_id::text,
	coalesce_count, allow_ucs2, credit_source, credit_day, attempts, max_attempts, created_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.ConversationID, &j.Kind, &j.Status, &j.LatestMessageID, &j.ReplyMessageID,
		&j.CoalesceCount, &j.AllowUCS2, &j.CreditSource, &j.CreditDay, &j.Attempts, &j.MaxAttempts, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

type EnqueueOptions struct {
	Debounce    time.Duration // hold for a new job
	Extend      time.Duration // extension when coalescing into a pending job
	MaxCoalesce int
	MaxAttempts int
	AllowUCS2   bool
}

// EnqueueAnswer implements the inbound debounce/coalescing strategy
// (plan §8): with no pending answer job, insert one available after
// Debounce; otherwise attach the message to the pending job and push its
// start back by Extend (at most MaxCoalesce times, so a chatty sender
// cannot postpone the answer forever).
//
// Must run in the same transaction as InsertInbound: that statement locks
// the conversation row, which serialises concurrent webhooks per user.
func (q *Q) EnqueueAnswer(ctx context.Context, convID, messageID string, o EnqueueOptions) (jobID string, coalesced bool, err error) {
	var count int
	err = q.db.QueryRow(ctx, `SELECT id::text, coalesce_count FROM jobs
		WHERE conversation_id = $1 AND kind = 'answer' AND status = 'pending' FOR UPDATE`, convID).Scan(&jobID, &count)
	switch {
	case err == nil:
		extend := o.Extend
		if count >= o.MaxCoalesce {
			extend = 0
		}
		_, err = q.db.Exec(ctx, `UPDATE jobs SET
				coalesce_count = coalesce_count + 1,
				available_at = GREATEST(available_at, now() + make_interval(secs => $2)),
				latest_message_id = $3, allow_ucs2 = allow_ucs2 OR $4, updated_at = now()
			WHERE id = $1`, jobID, seconds(extend), messageID, o.AllowUCS2)
		coalesced = true
	case errors.Is(err, pgx.ErrNoRows):
		err = q.db.QueryRow(ctx, `INSERT INTO jobs (conversation_id, kind, status, latest_message_id, allow_ucs2, max_attempts, available_at)
			VALUES ($1, 'answer', 'pending', $2, $3, $4, now() + make_interval(secs => $5)) RETURNING id::text`,
			convID, messageID, o.AllowUCS2, o.MaxAttempts, seconds(o.Debounce)).Scan(&jobID)
	}
	if err != nil {
		return "", false, err
	}
	return jobID, coalesced, q.AttachToJob(ctx, messageID, jobID)
}

// CreateSendJob queues delivery of an already stored outbound message.
func (q *Q) CreateSendJob(ctx context.Context, convID, messageID string, maxAttempts int) (string, error) {
	var id string
	err := q.db.QueryRow(ctx, `INSERT INTO jobs (conversation_id, kind, status, reply_message_id, max_attempts, available_at)
		VALUES ($1, 'send', 'pending', $2, $3, now()) RETURNING id::text`, convID, messageID, maxAttempts).Scan(&id)
	return id, err
}

// ClaimJob leases the next runnable job (FOR UPDATE SKIP LOCKED). A job is
// runnable when it is pending and matured, or running with an expired lease
// (crashed worker or scheduled retry). Answer jobs for one conversation run
// one at a time so replies keep their order: an answer job waits while
// another one of its conversation holds a live lease, or is an earlier job
// still running with attempts left (a due retry or a crashed attempt).
//
// The second rule closes a race. Claiming a due retry only renews its
// lease, which a concurrent claimer's snapshot cannot see, so when only a
// live lease blocked, a second worker could take the newer pending job
// while the retry was being claimed and both ran at once. "Running with
// attempts left" holds before and after the claim. A job that used its last
// attempt is never claimed again and stops blocking once its lease lapses.
// The pending job is always the newest, since a conversation has at most
// one; between two running jobs the older goes first.
func (q *Q) ClaimJob(ctx context.Context, owner string, lease time.Duration) (*Job, error) {
	j, err := scanJob(q.db.QueryRow(ctx, `
		WITH next AS (
			SELECT j.id FROM jobs j
			WHERE ((j.status = 'pending' AND j.available_at <= now())
			    OR (j.status = 'running' AND j.lease_expires_at < now() AND j.attempts < j.max_attempts))
			  AND (j.kind <> 'answer' OR NOT EXISTS (
			        SELECT 1 FROM jobs r
			        WHERE r.conversation_id = j.conversation_id AND r.id <> j.id AND r.kind = 'answer'
			          AND r.status = 'running'
			          AND (r.lease_expires_at >= now()
			            OR (r.attempts < r.max_attempts
			                AND (j.status = 'pending' OR (r.created_at, r.id) < (j.created_at, j.id))))))
			ORDER BY j.available_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs SET status = 'running', attempts = jobs.attempts + 1, lease_owner = $1,
			lease_expires_at = now() + make_interval(secs => $2), updated_at = now()
		FROM next WHERE jobs.id = next.id
		RETURNING jobs.id::text, jobs.conversation_id::text, jobs.kind, jobs.status, jobs.latest_message_id::text,
			jobs.reply_message_id::text, jobs.coalesce_count, jobs.allow_ucs2, jobs.credit_source, jobs.credit_day,
			jobs.attempts, jobs.max_attempts, jobs.created_at`, owner, seconds(lease)))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (q *Q) GetJob(ctx context.Context, id string) (Job, error) {
	return scanJob(q.db.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
}

func (q *Q) CompleteJob(ctx context.Context, id, result string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET status = 'done', result = $2, completed_at = now(),
		lease_owner = NULL, lease_expires_at = NULL, updated_at = now() WHERE id = $1`, id, result)
	return err
}

func (q *Q) FailJob(ctx context.Context, id, result, lastError string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET status = 'failed', result = $2, last_error = $3, completed_at = now(),
		lease_owner = NULL, lease_expires_at = NULL, updated_at = now() WHERE id = $1`, id, result, lastError)
	return err
}

// RetryJob schedules another attempt. The job stays 'running' with its
// lease set to the retry time, so it keeps its place in conversation order
// and a newer pending job waits behind it (see ClaimJob).
func (q *Q) RetryJob(ctx context.Context, id string, delay time.Duration, lastError string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET lease_owner = NULL, lease_expires_at = now() + make_interval(secs => $2),
		last_error = $3, updated_at = now() WHERE id = $1`, id, seconds(delay), lastError)
	return err
}

// CompleteIfPending finishes a job only if no worker has claimed it (used
// when the USSD fast path answered in time).
func (q *Q) CompleteIfPending(ctx context.Context, id, result string) (bool, error) {
	tag, err := q.db.Exec(ctx, `UPDATE jobs SET status = 'done', result = $2, completed_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id, result)
	return tag.RowsAffected() == 1, err
}

func (q *Q) CancelPendingJob(ctx context.Context, id, result string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET status = 'cancelled', result = $2, completed_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id, result)
	return err
}

// MakeAvailableNow hands a pending job to the workers immediately.
func (q *Q) MakeAvailableNow(ctx context.Context, id string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET available_at = now(), updated_at = now() WHERE id = $1 AND status = 'pending'`, id)
	return err
}

func (q *Q) SetJobReply(ctx context.Context, jobID, messageID string) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET reply_message_id = $2, updated_at = now() WHERE id = $1`, jobID, messageID)
	return err
}

func (q *Q) SetJobCredit(ctx context.Context, jobID, source string, day time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE jobs SET credit_source = $2, credit_day = $3, updated_at = now() WHERE id = $1`,
		jobID, nullIfEmpty(source), day)
	return err
}

// FailExhaustedJobs marks running jobs whose lease expired after their last
// attempt as failed.
func (q *Q) FailExhaustedJobs(ctx context.Context) (int64, error) {
	tag, err := q.db.Exec(ctx, `UPDATE jobs SET status = 'failed', result = coalesce(result, 'exhausted'),
		completed_at = now(), lease_owner = NULL, updated_at = now()
		WHERE status = 'running' AND lease_expires_at < now() AND attempts >= max_attempts`)
	return tag.RowsAffected(), err
}

const notifyChannel = "smsai_jobs"

// NotifyChannel is the LISTEN/NOTIFY channel that wakes workers.
func NotifyChannel() string { return notifyChannel }

// NotifyJobs wakes idle workers (delivered on commit when called in a Tx).
func (q *Q) NotifyJobs(ctx context.Context) error {
	_, err := q.db.Exec(ctx, `SELECT pg_notify($1, '')`, notifyChannel)
	return err
}
