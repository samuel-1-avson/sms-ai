package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"smsaighana/internal/security"
)

// Message kinds.
const (
	KindQuestion     = "question"
	KindCommand      = "command"
	KindAnswer       = "answer"
	KindMore         = "more"
	KindCommandReply = "command_reply"
	KindNotice       = "notice"
	KindSaved        = "saved" // USSD answer re-sent by SMS on request
)

// Message statuses.
const (
	StatusReceived       = "received"
	StatusRateLimited    = "rate_limited"
	StatusIgnored        = "ignored"
	StatusOverLimit      = "over_limit"
	StatusQueued         = "queued"
	StatusSending        = "sending"
	StatusSubmitted      = "submitted"
	StatusDelivered      = "delivered"
	StatusFailed         = "failed"
	StatusUnknownTimeout = "unknown_timeout"
	StatusBlockedCap     = "blocked_cap"
	StatusBlockedOff     = "blocked_disabled"
	StatusDisplayed      = "displayed" // shown inline on a USSD screen
)

type InboundMessage struct {
	ConversationID    string
	Provider          string
	ProviderMessageID string
	Kind              string
	Body              string
	DeliveryChannel   string
	Status            string
}

func (q *Q) nextSequence(ctx context.Context, convID string) (int64, error) {
	var seq int64
	err := q.db.QueryRow(ctx, `UPDATE conversations SET next_sequence = next_sequence + 1 WHERE id = $1
		RETURNING next_sequence - 1`, convID).Scan(&seq)
	return seq, err
}

// InsertInbound stores an inbound message. duplicate is true when the same
// (provider, provider_message_id) was already stored: the webhook is a retry
// and must not trigger any work (plan §5.4).
func (q *Q) InsertInbound(ctx context.Context, m InboundMessage) (id string, duplicate bool, err error) {
	seq, err := q.nextSequence(ctx, m.ConversationID)
	if err != nil {
		return "", false, err
	}
	ct, ver, err := q.encrypt(security.PurposeMessage, m.Body)
	if err != nil {
		return "", false, err
	}
	err = q.db.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, sequence, provider_name, provider_message_id, direction, kind,
			delivery_channel, body_ciphertext, key_version, status)
		VALUES ($1, $2, $3, $4, 'inbound', $5, $6, $7, $8, $9)
		ON CONFLICT (provider_name, provider_message_id) WHERE direction = 'inbound' AND provider_message_id IS NOT NULL
		DO NOTHING
		RETURNING id::text`,
		m.ConversationID, seq, m.Provider, nullIfEmpty(m.ProviderMessageID), m.Kind, m.DeliveryChannel, ct, ver, m.Status,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", true, nil
	}
	return id, false, err
}

func (q *Q) SetMessageStatus(ctx context.Context, id, status string) error {
	_, err := q.db.Exec(ctx, `UPDATE messages SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

type OutboundMessage struct {
	ConversationID  string
	JobID           string
	Provider        string // sender that will deliver it ('ussd' gateway for inline)
	Kind            string
	Body            string
	DeliveryChannel string
	Status          string
	AnswerSource    string
	Encoding        string
	Units           int
	Segments        int
}

func (q *Q) InsertOutbound(ctx context.Context, m OutboundMessage) (string, error) {
	seq, err := q.nextSequence(ctx, m.ConversationID)
	if err != nil {
		return "", err
	}
	ct, ver, err := q.encrypt(security.PurposeMessage, m.Body)
	if err != nil {
		return "", err
	}
	var id string
	err = q.db.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, sequence, job_id, provider_name, direction, kind, delivery_channel,
			body_ciphertext, key_version, encoding_type, septet_or_char_count, segment_count, answer_source, status)
		VALUES ($1, $2, $3, $4, 'outbound', $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id::text`,
		m.ConversationID, seq, nullIfEmpty(m.JobID), m.Provider, m.Kind, m.DeliveryChannel, ct, ver,
		nullIfEmpty(m.Encoding), m.Units, m.Segments, nullIfEmpty(m.AnswerSource), m.Status,
	).Scan(&id)
	return id, err
}

// DispatchInfo is everything the dispatcher needs to send an outbound SMS.
type DispatchInfo struct {
	ID                string
	ConversationID    string
	UserID            string
	ConvProvider      string
	ConvChannel       string
	ServiceID         string
	Provider          string
	Kind              string
	Body              string
	Status            string
	Encoding          string
	Segments          int
	ProviderMessageID string
}

// GetForDispatch loads an outbound message, locking it when forUpdate.
func (q *Q) GetForDispatch(ctx context.Context, id string, forUpdate bool) (DispatchInfo, error) {
	sql := `SELECT m.id::text, m.conversation_id::text, c.user_id::text, c.provider_name, c.channel_type, c.service_id,
			m.provider_name, m.kind, m.body_ciphertext, m.key_version, m.status, coalesce(m.encoding_type, 'gsm7'),
			m.segment_count, coalesce(m.provider_message_id, '')
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = $1 AND m.direction = 'outbound'`
	if forUpdate {
		sql += ` FOR UPDATE OF m`
	}
	var d DispatchInfo
	var ct []byte
	var ver int
	err := q.db.QueryRow(ctx, sql, id).Scan(&d.ID, &d.ConversationID, &d.UserID, &d.ConvProvider, &d.ConvChannel, &d.ServiceID,
		&d.Provider, &d.Kind, &ct, &ver, &d.Status, &d.Encoding, &d.Segments, &d.ProviderMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Body, err = q.decrypt(security.PurposeMessage, ct, ver)
	return d, err
}

func (q *Q) UpdateOutbound(ctx context.Context, id, status, providerMessageID, errorCode string) error {
	_, err := q.db.Exec(ctx, `UPDATE messages SET status = $2,
		provider_message_id = coalesce($3, provider_message_id), error_code = coalesce($4, error_code), updated_at = now()
		WHERE id = $1`, id, status, nullIfEmpty(providerMessageID), nullIfEmpty(errorCode))
	return err
}

// ApplyReceipt records a delivery receipt idempotently: repeated or stale
// receipts never move a message to a lower-ranked status.
func (q *Q) ApplyReceipt(ctx context.Context, provider, providerMessageID, status, errorCode string) (bool, error) {
	tag, err := q.db.Exec(ctx, `UPDATE messages SET status = $3, error_code = coalesce($4, error_code), updated_at = now()
		WHERE provider_name = $1 AND provider_message_id = $2 AND direction = 'outbound'
		  AND message_status_rank(status) < message_status_rank($3)`,
		provider, providerMessageID, status, nullIfEmpty(errorCode))
	return tag.RowsAffected() > 0, err
}

type Turn struct {
	Role    string // user | assistant
	Kind    string
	Content string
}

// RecentTurns returns up to limit question/answer messages of the current
// context window, oldest first, excluding messages that belong to the job
// being answered.
func (q *Q) RecentTurns(ctx context.Context, convID, excludeJobID string, since time.Time, limit int) ([]Turn, error) {
	rows, err := q.db.Query(ctx, `
		SELECT direction, kind, body_ciphertext, key_version FROM messages
		WHERE conversation_id = $1 AND created_at >= $2
		  AND kind IN ('question', 'answer', 'more')
		  AND ($3::uuid IS NULL OR job_id IS DISTINCT FROM $3::uuid)
		  AND status NOT IN ('rate_limited', 'ignored', 'over_limit', 'failed', 'blocked_cap', 'blocked_disabled')
		ORDER BY sequence DESC
		LIMIT $4`, convID, since, nullIfEmpty(excludeJobID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []Turn
	for rows.Next() {
		var dir, kind string
		var ct []byte
		var ver int
		if err := rows.Scan(&dir, &kind, &ct, &ver); err != nil {
			return nil, err
		}
		body, err := q.decrypt(security.PurposeMessage, ct, ver)
		if err != nil {
			return nil, err
		}
		role := "assistant"
		if dir == "inbound" {
			role = "user"
		}
		turns = append(turns, Turn{Role: role, Kind: kind, Content: body})
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, rows.Err()
}

// JobQuestion joins the inbound texts coalesced into a job, in order.
func (q *Q) JobQuestion(ctx context.Context, jobID string) (string, error) {
	rows, err := q.db.Query(ctx, `SELECT body_ciphertext, key_version FROM messages
		WHERE job_id = $1 AND direction = 'inbound' ORDER BY sequence`, jobID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var ct []byte
		var ver int
		if err := rows.Scan(&ct, &ver); err != nil {
			return "", err
		}
		body, err := q.decrypt(security.PurposeMessage, ct, ver)
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(body); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " "), rows.Err()
}

func (q *Q) AttachToJob(ctx context.Context, messageID, jobID string) error {
	_, err := q.db.Exec(ctx, `UPDATE messages SET job_id = $2 WHERE id = $1`, messageID, jobID)
	return err
}

// CountRecentInbound counts a user's inbound messages since t (rate limit).
func (q *Q) CountRecentInbound(ctx context.Context, userID string, since time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_id = $1 AND m.direction = 'inbound' AND m.created_at >= $2`, userID, since).Scan(&n)
	return n, err
}

type OutboxEntry struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Body            string    `json:"body"`
	Status          string    `json:"status"`
	Channel         string    `json:"conversation_channel"`
	DeliveryChannel string    `json:"delivery_channel"`
	Segments        int       `json:"segments"`
	Encoding        string    `json:"encoding"`
	Source          string    `json:"answer_source"`
	CreatedAt       time.Time `json:"created_at"`
}

// Outbox lists recent outbound messages for a user (development simulator).
func (q *Q) Outbox(ctx context.Context, userID string, limit int) ([]OutboxEntry, error) {
	rows, err := q.db.Query(ctx, `
		SELECT m.id::text, m.kind, m.body_ciphertext, m.key_version, m.status, c.channel_type, m.delivery_channel,
			m.segment_count, coalesce(m.encoding_type, ''), coalesce(m.answer_source, ''), m.created_at
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_id = $1 AND m.direction = 'outbound'
		ORDER BY m.created_at DESC, m.sequence DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		var ct []byte
		var ver int
		if err := rows.Scan(&e.ID, &e.Kind, &ct, &ver, &e.Status, &e.Channel, &e.DeliveryChannel, &e.Segments, &e.Encoding, &e.Source, &e.CreatedAt); err != nil {
			return nil, err
		}
		if e.Body, err = q.decrypt(security.PurposeMessage, ct, ver); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
