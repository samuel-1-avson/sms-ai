package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"smsaighana/internal/grounding"
	"smsaighana/internal/security"
)

// ---------------------------------------------------------------------------
// Grounding snippets
// ---------------------------------------------------------------------------

// UpsertSnippets writes seed snippets, replacing rows with the same slug.
func (q *Q) UpsertSnippets(ctx context.Context, snippets []grounding.Snippet) (int, error) {
	n := 0
	for _, s := range snippets {
		_, err := q.db.Exec(ctx, `
			INSERT INTO knowledge_snippets (slug, category, trigger_keywords, verified_answer_gsm7, context_note,
				direct_reply_enabled, reviewed, source, enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (slug) DO UPDATE SET category = EXCLUDED.category, trigger_keywords = EXCLUDED.trigger_keywords,
				verified_answer_gsm7 = EXCLUDED.verified_answer_gsm7, context_note = EXCLUDED.context_note,
				direct_reply_enabled = EXCLUDED.direct_reply_enabled, reviewed = EXCLUDED.reviewed,
				source = EXCLUDED.source, enabled = EXCLUDED.enabled, updated_at = now()`,
			s.Slug, s.Category, strings.Join(s.Triggers, "\n"), s.Answer, nullIfEmpty(s.Context),
			s.DirectReply, s.Reviewed, nullIfEmpty(s.Source), s.IsEnabled())
		if err != nil {
			return n, fmt.Errorf("snippet %s: %w", s.Slug, err)
		}
		n++
	}
	return n, nil
}

// LoadSnippets returns enabled snippets; reviewedOnly drops unverified ones.
func (q *Q) LoadSnippets(ctx context.Context, reviewedOnly bool) ([]grounding.Snippet, error) {
	rows, err := q.db.Query(ctx, `SELECT slug, category, trigger_keywords, verified_answer_gsm7, coalesce(context_note, ''),
			direct_reply_enabled, reviewed, coalesce(source, '')
		FROM knowledge_snippets WHERE enabled AND (reviewed OR NOT $1) ORDER BY slug`, reviewedOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []grounding.Snippet
	for rows.Next() {
		var s grounding.Snippet
		var triggers string
		if err := rows.Scan(&s.Slug, &s.Category, &triggers, &s.Answer, &s.Context, &s.DirectReply, &s.Reviewed, &s.Source); err != nil {
			return nil, err
		}
		for _, t := range strings.Split(triggers, "\n") {
			if t = strings.TrimSpace(t); t != "" {
				s.Triggers = append(s.Triggers, t)
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type SnippetSearchHit struct {
	Slug     string  `json:"slug"`
	Category string  `json:"category"`
	Answer   string  `json:"answer"`
	Reviewed bool    `json:"reviewed"`
	Score    float64 `json:"score"`
}

// SearchSnippets runs a pg_trgm word-similarity search (admin curation aid).
func (q *Q) SearchSnippets(ctx context.Context, query string, limit int) ([]SnippetSearchHit, error) {
	rows, err := q.db.Query(ctx, `SELECT slug, category, verified_answer_gsm7, reviewed,
			word_similarity($1, trigger_keywords)::float8 AS score
		FROM knowledge_snippets WHERE enabled AND $1 <% trigger_keywords
		ORDER BY score DESC LIMIT $2`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnippetSearchHit
	for rows.Next() {
		var h SnippetSearchHit
		if err := rows.Scan(&h.Slug, &h.Category, &h.Answer, &h.Reviewed, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Kill switches
// ---------------------------------------------------------------------------

const (
	FlagAIEnabled          = "ai_enabled"
	FlagSMSOutboundEnabled = "sms_outbound_enabled"
)

func (q *Q) Flags(ctx context.Context) (map[string]bool, error) {
	rows, err := q.db.Query(ctx, `SELECT key, value FROM system_flags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{FlagAIEnabled: true, FlagSMSOutboundEnabled: true}
	for rows.Next() {
		var k string
		var v bool
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (q *Q) SetFlag(ctx context.Context, key string, value bool) error {
	if key != FlagAIEnabled && key != FlagSMSOutboundEnabled {
		return fmt.Errorf("unknown flag %q", key)
	}
	_, err := q.db.Exec(ctx, `INSERT INTO system_flags (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// USSD sessions
// ---------------------------------------------------------------------------

type USSDSession struct {
	Provider       string
	SessionID      string
	UserID         string
	ConversationID string
	State          string
	LastAnswer     string
}

func (q *Q) GetUSSDSession(ctx context.Context, provider, sessionID string) (USSDSession, bool, error) {
	var s USSDSession
	var ct []byte
	var ver *int
	err := q.db.QueryRow(ctx, `SELECT provider_name, session_id, user_id::text, conversation_id::text, state,
			last_answer_ciphertext, last_answer_key_version
		FROM ussd_sessions WHERE provider_name = $1 AND session_id = $2 AND expires_at > now()`, provider, sessionID).
		Scan(&s.Provider, &s.SessionID, &s.UserID, &s.ConversationID, &s.State, &ct, &ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if ct != nil && ver != nil {
		if s.LastAnswer, err = q.decrypt(security.PurposeUSSDAnswer, ct, *ver); err != nil {
			return s, false, err
		}
	}
	return s, true, nil
}

func (q *Q) SaveUSSDSession(ctx context.Context, s USSDSession, ttl time.Duration) error {
	var ct []byte
	var ver *int
	if s.LastAnswer != "" {
		c, v, err := q.encrypt(security.PurposeUSSDAnswer, s.LastAnswer)
		if err != nil {
			return err
		}
		ct, ver = c, &v
	}
	_, err := q.db.Exec(ctx, `
		INSERT INTO ussd_sessions (provider_name, session_id, user_id, conversation_id, state,
			last_answer_ciphertext, last_answer_key_version, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8))
		ON CONFLICT (provider_name, session_id) DO UPDATE SET state = EXCLUDED.state,
			conversation_id = EXCLUDED.conversation_id,
			last_answer_ciphertext = EXCLUDED.last_answer_ciphertext,
			last_answer_key_version = EXCLUDED.last_answer_key_version,
			expires_at = EXCLUDED.expires_at, updated_at = now()`,
		s.Provider, s.SessionID, s.UserID, s.ConversationID, s.State, ct, ver, seconds(ttl))
	return err
}

// ---------------------------------------------------------------------------
// Model calls and send attempts
// ---------------------------------------------------------------------------

type ModelCall struct {
	JobID           string
	MessageID       string
	Channel         string
	Model           string
	Status          string
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	CostUSD         float64
	LatencyMS       int
	Error           string
}

func (q *Q) InsertModelCall(ctx context.Context, m ModelCall) error {
	_, err := q.db.Exec(ctx, `INSERT INTO model_calls (job_id, message_id, channel, model_name, status, input_tokens,
			output_tokens, reasoning_tokens, estimated_cost_usd, latency_ms, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11)`,
		nullIfEmpty(m.JobID), nullIfEmpty(m.MessageID), m.Channel, m.Model, m.Status, m.InputTokens, m.OutputTokens,
		m.ReasoningTokens, m.CostUSD, m.LatencyMS, nullIfEmpty(m.Error))
	return err
}

func (q *Q) LinkModelCalls(ctx context.Context, jobID, messageID string) error {
	_, err := q.db.Exec(ctx, `UPDATE model_calls SET message_id = $2 WHERE job_id = $1 AND message_id IS NULL`, jobID, messageID)
	return err
}

type SendAttempt struct {
	ID                string
	MessageID         string
	Provider          string
	RequestID         string
	ProviderMessageID string
	Status            string
	CreatedAt         time.Time
}

func (q *Q) InsertSendAttempt(ctx context.Context, messageID, provider, requestID string) (string, error) {
	var id string
	err := q.db.QueryRow(ctx, `INSERT INTO send_attempts (message_id, provider_name, provider_request_id, status)
		VALUES ($1, $2, $3, 'pending') RETURNING id::text`, messageID, provider, requestID).Scan(&id)
	return id, err
}

func (q *Q) UpdateSendAttempt(ctx context.Context, id, status, providerMessageID, errMsg string) error {
	_, err := q.db.Exec(ctx, `UPDATE send_attempts SET status = $2, provider_message_id = coalesce($3, provider_message_id),
		error = coalesce($4, error), updated_at = now() WHERE id = $1`, id, status, nullIfEmpty(providerMessageID), nullIfEmpty(errMsg))
	return err
}

// OpenAttempts lists attempts whose outcome is not known, oldest first. With
// messageID set it is restricted to that message.
func (q *Q) OpenAttempts(ctx context.Context, messageID string, olderThan time.Duration, limit int) ([]SendAttempt, error) {
	rows, err := q.db.Query(ctx, `SELECT id::text, message_id::text, provider_name, provider_request_id,
			coalesce(provider_message_id, ''), status, created_at
		FROM send_attempts
		WHERE status IN ('pending', 'unknown_timeout') AND ($1::uuid IS NULL OR message_id = $1::uuid)
		  AND created_at <= now() - make_interval(secs => $2)
		ORDER BY created_at LIMIT $3`, nullIfEmpty(messageID), seconds(olderThan), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SendAttempt
	for rows.Next() {
		var a SendAttempt
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Provider, &a.RequestID, &a.ProviderMessageID, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

type PurgeResult struct {
	Messages     int64
	USSDSessions int64
	Overflow     int64
	Jobs         int64
	ModelCalls   int64
}

// Purge enforces the retention policy (plan §10.2): messages older than
// retentionDays, expired USSD sessions and MORE buffers, and old job rows.
func (q *Q) Purge(ctx context.Context, retentionDays int) (PurgeResult, error) {
	var r PurgeResult
	steps := []struct {
		sql string
		dst *int64
	}{
		{`DELETE FROM messages WHERE created_at < now() - make_interval(days => $1)`, &r.Messages},
		{`DELETE FROM jobs WHERE status IN ('done', 'failed', 'cancelled') AND updated_at < now() - make_interval(days => $1)`, &r.Jobs},
		{`DELETE FROM model_calls WHERE created_at < now() - make_interval(days => $1)`, &r.ModelCalls},
		{`DELETE FROM ussd_sessions WHERE expires_at < now() AND $1 >= 0`, &r.USSDSessions},
		{`UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL
			WHERE overflow_expires_at < now() AND $1 >= 0`, &r.Overflow},
	}
	for _, s := range steps {
		tag, err := q.db.Exec(ctx, s.sql, retentionDays)
		if err != nil {
			return r, err
		}
		*s.dst = tag.RowsAffected()
	}
	return r, nil
}
