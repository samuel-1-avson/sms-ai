package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"smsaighana/internal/security"
)

type Conversation struct {
	ID               string
	UserID           string
	ServiceID        string
	Channel          string // sms | ussd
	Provider         string
	ContextStartedAt time.Time
	ContextExpiresAt time.Time
	HasOverflow      bool
}

const convCols = `id::text, user_id::text, service_id, channel_type, provider_name, context_started_at, context_expires_at,
	(overflow_buffer IS NOT NULL AND overflow_expires_at > now())`

func scanConversation(row pgx.Row) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.UserID, &c.ServiceID, &c.Channel, &c.Provider, &c.ContextStartedAt, &c.ContextExpiresAt, &c.HasOverflow)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// TouchConversation gets or creates the conversation for (user, service,
// channel) and extends its context window. If the window had lapsed
// (24h of inactivity by default) a fresh context starts now.
func (q *Q) TouchConversation(ctx context.Context, userID, serviceID, channel, provider string, ttl time.Duration) (Conversation, error) {
	return scanConversation(q.db.QueryRow(ctx, `
		INSERT INTO conversations (user_id, service_id, channel_type, provider_name, context_started_at, context_expires_at)
		VALUES ($1, $2, $3, $4, now(), now() + make_interval(secs => $5))
		ON CONFLICT (user_id, service_id, channel_type) DO UPDATE SET
			provider_name      = EXCLUDED.provider_name,
			context_started_at = CASE WHEN conversations.context_expires_at < now() THEN now()
			                          ELSE conversations.context_started_at END,
			context_expires_at = now() + make_interval(secs => $5),
			last_activity_at   = now(),
			updated_at         = now()
		RETURNING `+convCols, userID, serviceID, channel, provider, seconds(ttl)))
}

func (q *Q) GetConversation(ctx context.Context, id string) (Conversation, error) {
	return scanConversation(q.db.QueryRow(ctx, `SELECT `+convCols+` FROM conversations WHERE id = $1`, id))
}

// ResetContext implements NEW: history before now is no longer sent to the
// model, and any MORE buffer is dropped.
func (q *Q) ResetContext(ctx context.Context, convID string) error {
	_, err := q.db.Exec(ctx, `UPDATE conversations SET context_started_at = now(), context_expires_at = now(),
		overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL, updated_at = now() WHERE id = $1`, convID)
	return err
}

// Overflow is the remainder of the last answer, served page by page by MORE.
type Overflow struct {
	Rest      string `json:"rest"`
	PagesLeft int    `json:"pages_left"`
}

// SetOverflow stores (or clears, when empty) the MORE buffer.
func (q *Q) SetOverflow(ctx context.Context, convID string, o Overflow, ttl time.Duration) error {
	if o.Rest == "" || o.PagesLeft <= 0 {
		_, err := q.db.Exec(ctx, `UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL,
			overflow_expires_at = NULL, updated_at = now() WHERE id = $1`, convID)
		return err
	}
	b, _ := json.Marshal(o)
	ct, ver, err := q.kr.Encrypt(security.PurposeOverflow, b)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `UPDATE conversations SET overflow_buffer = $2, overflow_key_version = $3,
		overflow_expires_at = now() + make_interval(secs => $4), updated_at = now() WHERE id = $1`, convID, ct, ver, seconds(ttl))
	return err
}

// LockOverflow reads the MORE buffer with a row lock; call inside Tx and
// follow with SetOverflow.
func (q *Q) LockOverflow(ctx context.Context, convID string) (Overflow, bool, error) {
	var ct []byte
	var ver *int
	var live bool
	err := q.db.QueryRow(ctx, `SELECT overflow_buffer, overflow_key_version, coalesce(overflow_expires_at > now(), false)
		FROM conversations WHERE id = $1 FOR UPDATE`, convID).Scan(&ct, &ver, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Overflow{}, false, ErrNotFound
	}
	if err != nil || ct == nil || ver == nil || !live {
		return Overflow{}, false, err
	}
	b, err := q.kr.Decrypt(security.PurposeOverflow, ct, *ver)
	if err != nil {
		return Overflow{}, false, err
	}
	var o Overflow
	if err := json.Unmarshal(b, &o); err != nil {
		return Overflow{}, false, err
	}
	return o, o.Rest != "", nil
}
