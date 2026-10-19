package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrVoucherInvalid   = errors.New("voucher: unknown code")
	ErrVoucherExpired   = errors.New("voucher: expired or inactive")
	ErrVoucherExhausted = errors.New("voucher: pool exhausted")
	ErrVoucherUsed      = errors.New("voucher: already redeemed by this user")
)

type Pool struct {
	ID           string     `json:"id"`
	VoucherCode  string     `json:"voucher_code"`
	SponsorName  string     `json:"sponsor_name"`
	MaxSegments  int        `json:"max_segments"`
	UsedSegments int        `json:"used_segments"`
	PerUserGrant int        `json:"per_user_grant"`
	Active       bool       `json:"active"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	Redemptions  int        `json:"redemptions"`
}

// RedeemVoucher grants a pool's per-user credit to the user, once per pool,
// in one transaction with the pool row locked (plan §11.3).
func (q *Q) RedeemVoucher(ctx context.Context, userID, code string) (granted, balance int, err error) {
	var poolID string
	var grant, max, used int
	var active bool
	var expires *time.Time
	err = q.db.QueryRow(ctx, `SELECT id::text, per_user_grant, max_segments, used_segments, active, expires_at
		FROM sponsored_pools WHERE voucher_code = $1 FOR UPDATE`, strings.ToUpper(code)).
		Scan(&poolID, &grant, &max, &used, &active, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrVoucherInvalid
	}
	if err != nil {
		return 0, 0, err
	}
	if !active || expires != nil && time.Now().After(*expires) {
		return 0, 0, ErrVoucherExpired
	}
	if used+grant > max {
		return 0, 0, ErrVoucherExhausted
	}
	tag, err := q.db.Exec(ctx, `INSERT INTO voucher_redemptions (pool_id, user_id, granted_segments) VALUES ($1, $2, $3)
		ON CONFLICT (pool_id, user_id) DO NOTHING`, poolID, userID, grant)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, 0, ErrVoucherUsed
	}
	if _, err := q.db.Exec(ctx, `UPDATE sponsored_pools SET used_segments = used_segments + $2 WHERE id = $1`, poolID, grant); err != nil {
		return 0, 0, err
	}
	err = q.db.QueryRow(ctx, `UPDATE users SET bonus_sms_quota = bonus_sms_quota + $2, updated_at = now()
		WHERE id = $1 RETURNING bonus_sms_quota`, userID, grant).Scan(&balance)
	return grant, balance, err
}

func (q *Q) CreatePool(ctx context.Context, p Pool) (Pool, error) {
	err := q.db.QueryRow(ctx, `INSERT INTO sponsored_pools (voucher_code, sponsor_name, max_segments, per_user_grant, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text, active, created_at`,
		strings.ToUpper(p.VoucherCode), p.SponsorName, p.MaxSegments, p.PerUserGrant, p.ExpiresAt).
		Scan(&p.ID, &p.Active, &p.CreatedAt)
	p.VoucherCode = strings.ToUpper(p.VoucherCode)
	return p, err
}

func (q *Q) ListPools(ctx context.Context) ([]Pool, error) {
	rows, err := q.db.Query(ctx, `SELECT p.id::text, p.voucher_code, p.sponsor_name, p.max_segments, p.used_segments,
			p.per_user_grant, p.active, p.expires_at, p.created_at,
			(SELECT count(*) FROM voucher_redemptions r WHERE r.pool_id = p.id)
		FROM sponsored_pools p ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.VoucherCode, &p.SponsorName, &p.MaxSegments, &p.UsedSegments, &p.PerUserGrant,
			&p.Active, &p.ExpiresAt, &p.CreatedAt, &p.Redemptions); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (q *Q) SetPoolActive(ctx context.Context, code string, active bool) error {
	tag, err := q.db.Exec(ctx, `UPDATE sponsored_pools SET active = $2 WHERE voucher_code = $1`, strings.ToUpper(code), active)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
