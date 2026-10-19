package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	CreditFree  = "free"
	CreditBonus = "bonus"
)

func (q *Q) ensureUsageRow(ctx context.Context, userID string, day time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO usage_daily (user_id, day) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, day)
	return err
}

// HasCredit is a cheap pre-check before queueing a question.
func (q *Q) HasCredit(ctx context.Context, userID string, day time.Time, freeCap int) (bool, error) {
	var ok bool
	err := q.db.QueryRow(ctx, `SELECT
		coalesce((SELECT questions_used FROM usage_daily WHERE user_id = $1 AND day = $2), 0) < $3
		OR coalesce((SELECT bonus_sms_quota > 0 FROM users WHERE id = $1), false)`, userID, day, freeCap).Scan(&ok)
	return ok, err
}

// ConsumeCredit spends one question from the daily free allowance, then
// from sponsored bonus credit. source is "" when neither is available.
func (q *Q) ConsumeCredit(ctx context.Context, userID string, day time.Time, freeCap int) (source string, err error) {
	if err := q.ensureUsageRow(ctx, userID, day); err != nil {
		return "", err
	}
	var n int
	err = q.db.QueryRow(ctx, `UPDATE usage_daily SET questions_used = questions_used + 1
		WHERE user_id = $1 AND day = $2 AND questions_used < $3 RETURNING questions_used`, userID, day, freeCap).Scan(&n)
	if err == nil {
		return CreditFree, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = q.db.QueryRow(ctx, `UPDATE users SET bonus_sms_quota = bonus_sms_quota - 1, updated_at = now()
		WHERE id = $1 AND bonus_sms_quota > 0 RETURNING bonus_sms_quota`, userID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = q.db.Exec(ctx, `UPDATE usage_daily SET bonus_used = bonus_used + 1 WHERE user_id = $1 AND day = $2`, userID, day)
	return CreditBonus, err
}

// RefundCredit returns a credit when no answer could be produced.
func (q *Q) RefundCredit(ctx context.Context, userID, source string, day time.Time) error {
	var err error
	switch source {
	case CreditFree:
		_, err = q.db.Exec(ctx, `UPDATE usage_daily SET questions_used = greatest(questions_used - 1, 0)
			WHERE user_id = $1 AND day = $2`, userID, day)
	case CreditBonus:
		if _, err = q.db.Exec(ctx, `UPDATE users SET bonus_sms_quota = bonus_sms_quota + 1 WHERE id = $1`, userID); err == nil {
			_, err = q.db.Exec(ctx, `UPDATE usage_daily SET bonus_used = greatest(bonus_used - 1, 0)
				WHERE user_id = $1 AND day = $2`, userID, day)
		}
	}
	return err
}

func (q *Q) QuestionsUsed(ctx context.Context, userID string, day time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT coalesce((SELECT questions_used FROM usage_daily WHERE user_id = $1 AND day = $2), 0)`,
		userID, day).Scan(&n)
	return n, err
}

// markOnce flips a boolean usage flag; it returns true only the first time.
func (q *Q) markOnce(ctx context.Context, column, userID string, day time.Time) (bool, error) {
	if err := q.ensureUsageRow(ctx, userID, day); err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `UPDATE usage_daily SET `+column+` = true
		WHERE user_id = $1 AND day = $2 AND NOT `+column, userID, day)
	return tag.RowsAffected() == 1, err
}

// MarkLimitNotice records the once-per-day "Daily limit reached" notice.
func (q *Q) MarkLimitNotice(ctx context.Context, userID string, day time.Time) (bool, error) {
	return q.markOnce(ctx, "limit_notice_sent", userID, day)
}

// MarkOutageNotice records the once-per-day outage notice.
func (q *Q) MarkOutageNotice(ctx context.Context, userID string, day time.Time) (bool, error) {
	return q.markOnce(ctx, "outage_notice_sent", userID, day)
}

func (q *Q) FailedCodeAttempts(ctx context.Context, userID string, day time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT coalesce((SELECT failed_code_attempts FROM usage_daily WHERE user_id = $1 AND day = $2), 0)`,
		userID, day).Scan(&n)
	return n, err
}

func (q *Q) IncrFailedCode(ctx context.Context, userID string, day time.Time) error {
	if err := q.ensureUsageRow(ctx, userID, day); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE usage_daily SET failed_code_attempts = failed_code_attempts + 1
		WHERE user_id = $1 AND day = $2`, userID, day)
	return err
}

func (q *Q) ensureGlobalRow(ctx context.Context, day time.Time) error {
	_, err := q.db.Exec(ctx, `INSERT INTO global_usage_daily (day) VALUES ($1) ON CONFLICT DO NOTHING`, day)
	return err
}

// ReserveSegments atomically books n SMS segments against both the global
// daily segment cap and the global daily spend cap (plan §3 cost control).
func (q *Q) ReserveSegments(ctx context.Context, day time.Time, n, segmentCap int, segmentCost, spendCap float64) (bool, error) {
	if err := q.ensureGlobalRow(ctx, day); err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `UPDATE global_usage_daily SET sms_segments = sms_segments + $2
		WHERE day = $1 AND sms_segments + $2 <= $3 AND (sms_segments + $2) * $4::numeric + ai_cost_usd <= $5::numeric`,
		day, n, segmentCap, segmentCost, spendCap)
	return tag.RowsAffected() == 1, err
}

func (q *Q) ReleaseSegments(ctx context.Context, day time.Time, n int) error {
	_, err := q.db.Exec(ctx, `UPDATE global_usage_daily SET sms_segments = greatest(sms_segments - $2, 0) WHERE day = $1`, day, n)
	return err
}

func (q *Q) AddAICost(ctx context.Context, day time.Time, usd float64) error {
	if err := q.ensureGlobalRow(ctx, day); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE global_usage_daily SET ai_cost_usd = ai_cost_usd + $2::numeric WHERE day = $1`, day, usd)
	return err
}

// DaySpend returns today's booked segments and AI spend.
func (q *Q) DaySpend(ctx context.Context, day time.Time) (segments int, aiCost float64, err error) {
	err = q.db.QueryRow(ctx, `SELECT coalesce(sms_segments, 0), coalesce(ai_cost_usd, 0)::float8 FROM global_usage_daily WHERE day = $1`,
		day).Scan(&segments, &aiCost)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return segments, aiCost, err
}
