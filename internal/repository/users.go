package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"smsaighana/internal/security"
)

type User struct {
	ID         string
	KeyVersion int
	Carrier    string
	OptedOut   bool
	BonusQuota int
	CreatedAt  time.Time
}

const userCols = `id::text, key_version, carrier, opted_out, bonus_sms_quota, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.KeyVersion, &u.Carrier, &u.OptedOut, &u.BonusQuota, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// FindOrCreateUser resolves a phone number to its pseudonymous user. It
// matches the lookup hash under every configured key version, so users
// written before a key rotation are still found, and lazily re-keys them
// to the current version (plan §16 "Key Rotation").
func (q *Q) FindOrCreateUser(ctx context.Context, e164, carrier string) (User, bool, error) {
	hashes := q.kr.LookupHashes(e164)
	list := make([]string, len(hashes))
	for i, h := range hashes {
		list[i] = h.Hash
	}
	rows, err := q.db.Query(ctx, `SELECT `+userCols+`, phone_lookup_hash FROM users WHERE phone_lookup_hash = ANY($1)`, list)
	if err != nil {
		return User{}, false, err
	}
	var found *User
	for rows.Next() {
		var u User
		var hash string
		if err := rows.Scan(&u.ID, &u.KeyVersion, &u.Carrier, &u.OptedOut, &u.BonusQuota, &u.CreatedAt, &hash); err != nil {
			rows.Close()
			return User{}, false, err
		}
		for _, h := range hashes {
			if h.Hash == hash && h.Version == u.KeyVersion && (found == nil || u.KeyVersion == q.kr.CurrentVersion()) {
				u := u
				found = &u
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return User{}, false, err
	}

	current := q.kr.CurrentVersion()
	if found != nil {
		if found.KeyVersion != current {
			if err := q.rekeyUser(ctx, found.ID, found.KeyVersion, e164); err != nil {
				return User{}, false, err
			}
			found.KeyVersion = current
		}
		return *found, false, nil
	}

	ct, ver, err := q.encrypt(security.PurposePhone, e164)
	if err != nil {
		return User{}, false, err
	}
	hash, _ := q.kr.LookupHash(current, e164)
	u, err := scanUser(q.db.QueryRow(ctx, `
		INSERT INTO users (phone_ciphertext, phone_lookup_hash, key_version, carrier)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (phone_lookup_hash, key_version) DO NOTHING
		RETURNING `+userCols, ct, hash, ver, carrier))
	if errors.Is(err, ErrNotFound) {
		// A concurrent request created the user first.
		u, err = scanUser(q.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone_lookup_hash = $1 AND key_version = $2`, hash, current))
		return u, false, err
	}
	return u, err == nil, err
}

func (q *Q) rekeyUser(ctx context.Context, id string, oldVersion int, e164 string) error {
	ct, ver, err := q.encrypt(security.PurposePhone, e164)
	if err != nil {
		return err
	}
	hash, _ := q.kr.LookupHash(ver, e164)
	_, err = q.db.Exec(ctx, `UPDATE users SET phone_ciphertext = $2, phone_lookup_hash = $3, key_version = $4, updated_at = now()
		WHERE id = $1 AND key_version = $5`, id, ct, hash, ver, oldVersion)
	return err
}

func (q *Q) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(q.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// FindUserByPhone looks a user up without creating one.
func (q *Q) FindUserByPhone(ctx context.Context, e164 string) (User, error) {
	for _, h := range q.kr.LookupHashes(e164) {
		u, err := scanUser(q.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone_lookup_hash = $1 AND key_version = $2`, h.Hash, h.Version))
		if err == nil || !errors.Is(err, ErrNotFound) {
			return u, err
		}
	}
	return User{}, ErrNotFound
}

// UserPhone decrypts the user's E.164 number (needed to send MT SMS).
func (q *Q) UserPhone(ctx context.Context, userID string) (string, error) {
	var ct []byte
	var ver int
	err := q.db.QueryRow(ctx, `SELECT phone_ciphertext, key_version FROM users WHERE id = $1`, userID).Scan(&ct, &ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return q.decrypt(security.PurposePhone, ct, ver)
}

func (q *Q) SetOptOut(ctx context.Context, userID string, optedOut bool) error {
	_, err := q.db.Exec(ctx, `UPDATE users SET opted_out = $2,
		opted_out_at = CASE WHEN $2 THEN now() ELSE NULL END, updated_at = now() WHERE id = $1`, userID, optedOut)
	return err
}

// DeleteUserContent removes every stored message, overflow buffer and USSD
// session for the user (DELETE command). The pseudonymous user row, its
// opt-out flag and quota ledger stay so limits cannot be reset this way.
func (q *Q) DeleteUserContent(ctx context.Context, userID string) error {
	stmts := []string{
		`UPDATE jobs SET status = 'cancelled', result = 'user_deleted', updated_at = now()
			WHERE status = 'pending' AND conversation_id IN (SELECT id FROM conversations WHERE user_id = $1)`,
		`DELETE FROM messages WHERE conversation_id IN (SELECT id FROM conversations WHERE user_id = $1)`,
		`UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL,
			context_started_at = now(), context_expires_at = now(), updated_at = now() WHERE user_id = $1`,
		`DELETE FROM ussd_sessions WHERE user_id = $1`,
	}
	for _, s := range stmts {
		if _, err := q.db.Exec(ctx, s, userID); err != nil {
			return err
		}
	}
	return nil
}
