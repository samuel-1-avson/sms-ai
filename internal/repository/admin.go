package repository

import (
	"context"
	"time"

	"smsaighana/internal/security"
)

// UsageReport aggregates one UTC day for the operator (plan §1 criterion 7).
// It contains counts and costs only — never message content.
type UsageReport struct {
	Day                  string         `json:"day"`
	Questions            int            `json:"questions"`
	QuestionsByResult    map[string]int `json:"questions_by_result"`
	CoalescedMessages    int            `json:"coalesced_messages"`
	GroundingDirect      int            `json:"grounding_direct_answers"`
	USSDInline           int            `json:"ussd_inline_answers"`
	USSDSMSFallback      int            `json:"ussd_sms_fallback_answers"`
	USSDOffloadRatio     float64        `json:"ussd_offload_ratio"`
	MoreServed           int            `json:"more_served"`
	OutboundByStatus     map[string]int `json:"outbound_by_status"`
	SMSSegments          int            `json:"sms_segments_booked"`
	SMSSegmentCap        int            `json:"sms_segment_cap"`
	SMSCostUSD           float64        `json:"sms_cost_usd"`
	AICalls              int            `json:"ai_calls"`
	AIEmptyResponses     int            `json:"ai_empty_responses"`
	AIErrors             int            `json:"ai_errors"`
	AIAvgLatencyMS       float64        `json:"ai_avg_latency_ms"`
	AICostUSD            float64        `json:"ai_cost_usd"`
	TotalSpendUSD        float64        `json:"total_spend_usd"`
	SpendCapUSD          float64        `json:"spend_cap_usd"`
	UsersTotal           int            `json:"users_total"`
	UsersNew             int            `json:"users_new"`
	UsersOptedOut        int            `json:"users_opted_out"`
	VoucherRedemptions   int            `json:"voucher_redemptions"`
	VoucherCreditGranted int            `json:"voucher_credit_granted"`
}

func (q *Q) UsageReport(ctx context.Context, day time.Time, segmentCost, spendCap float64, segmentCap int) (UsageReport, error) {
	day = Day(day)
	end := day.Add(24 * time.Hour)
	r := UsageReport{Day: day.Format("2006-01-02"), QuestionsByResult: map[string]int{}, OutboundByStatus: map[string]int{},
		SMSSegmentCap: segmentCap, SpendCapUSD: spendCap}

	rows, err := q.db.Query(ctx, `SELECT coalesce(result, status), count(*), coalesce(sum(coalesce_count), 0)
		FROM jobs WHERE kind = 'answer' AND created_at >= $1 AND created_at < $2 GROUP BY 1`, day, end)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var k string
		var n, coalesced int
		if err := rows.Scan(&k, &n, &coalesced); err != nil {
			rows.Close()
			return r, err
		}
		r.QuestionsByResult[k] = n
		r.Questions += n
		r.CoalescedMessages += coalesced
	}
	rows.Close()

	rows, err = q.db.Query(ctx, `SELECT m.kind, m.delivery_channel, c.channel_type, m.status, coalesce(m.answer_source, ''), count(*)
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.direction = 'outbound' AND m.created_at >= $1 AND m.created_at < $2 GROUP BY 1, 2, 3, 4, 5`, day, end)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var kind, delivery, conv, status, source string
		var n int
		if err := rows.Scan(&kind, &delivery, &conv, &status, &source, &n); err != nil {
			rows.Close()
			return r, err
		}
		r.OutboundByStatus[status] += n
		if kind == KindMore {
			r.MoreServed += n
		}
		if kind == KindAnswer {
			if source == "grounding" {
				r.GroundingDirect += n
			}
			if conv == "ussd" && delivery == "ussd" {
				r.USSDInline += n
			}
			if conv == "ussd" && delivery == "sms" {
				r.USSDSMSFallback += n
			}
		}
	}
	rows.Close()
	if total := r.USSDInline + r.USSDSMSFallback; total > 0 {
		r.USSDOffloadRatio = float64(r.USSDInline) / float64(total)
	}

	if err := q.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'empty'), count(*) FILTER (WHERE status = 'error'),
			coalesce(avg(latency_ms), 0)::float8, coalesce(sum(estimated_cost_usd), 0)::float8
		FROM model_calls WHERE created_at >= $1 AND created_at < $2`, day, end).
		Scan(&r.AICalls, &r.AIEmptyResponses, &r.AIErrors, &r.AIAvgLatencyMS, &r.AICostUSD); err != nil {
		return r, err
	}
	segments, aiCost, err := q.DaySpend(ctx, day)
	if err != nil {
		return r, err
	}
	r.SMSSegments = segments
	r.SMSCostUSD = float64(segments) * segmentCost
	if aiCost > r.AICostUSD {
		r.AICostUSD = aiCost
	}
	r.TotalSpendUSD = r.SMSCostUSD + r.AICostUSD

	if err := q.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE created_at >= $1 AND created_at < $2),
			count(*) FILTER (WHERE opted_out) FROM users`, day, end).Scan(&r.UsersTotal, &r.UsersNew, &r.UsersOptedOut); err != nil {
		return r, err
	}
	err = q.db.QueryRow(ctx, `SELECT count(*), coalesce(sum(granted_segments), 0) FROM voucher_redemptions
		WHERE redeemed_at >= $1 AND redeemed_at < $2`, day, end).Scan(&r.VoucherRedemptions, &r.VoucherCreditGranted)
	return r, err
}

// RekeyStats counts rows moved to the current key version.
type RekeyStats struct {
	Users, Messages, Overflow, USSDSessions int
}

// Rekey re-encrypts every row still sealed with an older key version, in
// batches, so an old key can be retired after rotation (plan §16).
func (s *Store) Rekey(ctx context.Context, batch int) (RekeyStats, error) {
	var st RekeyStats
	cur := s.kr.CurrentVersion()
	for {
		n := 0
		err := s.Tx(ctx, func(q *Q) error {
			rows, err := q.db.Query(ctx, `SELECT id::text, phone_ciphertext, key_version FROM users
				WHERE key_version <> $1 LIMIT $2 FOR UPDATE SKIP LOCKED`, cur, batch)
			if err != nil {
				return err
			}
			type row struct {
				id  string
				ct  []byte
				ver int
			}
			var list []row
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.id, &r.ct, &r.ver); err != nil {
					rows.Close()
					return err
				}
				list = append(list, r)
			}
			rows.Close()
			for _, r := range list {
				phone, err := q.decrypt(security.PurposePhone, r.ct, r.ver)
				if err != nil {
					return err
				}
				if err := q.rekeyUser(ctx, r.id, r.ver, phone); err != nil {
					return err
				}
			}
			n = len(list)
			return nil
		})
		if err != nil {
			return st, err
		}
		st.Users += n
		if n < batch {
			break
		}
	}
	for {
		n, err := s.rekeyBlob(ctx, `messages`, `body_ciphertext`, `key_version`, security.PurposeMessage, batch)
		if err != nil {
			return st, err
		}
		st.Messages += n
		if n < batch {
			break
		}
	}
	for {
		n, err := s.rekeyBlob(ctx, `conversations`, `overflow_buffer`, `overflow_key_version`, security.PurposeOverflow, batch)
		if err != nil {
			return st, err
		}
		st.Overflow += n
		if n < batch {
			break
		}
	}
	for {
		n, err := s.rekeyUSSD(ctx, batch)
		if err != nil {
			return st, err
		}
		st.USSDSessions += n
		if n < batch {
			break
		}
	}
	return st, nil
}

func (s *Store) rekeyBlob(ctx context.Context, table, col, verCol, purpose string, batch int) (int, error) {
	n := 0
	err := s.Tx(ctx, func(q *Q) error {
		rows, err := q.db.Query(ctx, `SELECT id::text, `+col+`, `+verCol+` FROM `+table+`
			WHERE `+col+` IS NOT NULL AND `+verCol+` <> $1 LIMIT $2 FOR UPDATE SKIP LOCKED`, s.kr.CurrentVersion(), batch)
		if err != nil {
			return err
		}
		type row struct {
			id  string
			ct  []byte
			ver int
		}
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.ct, &r.ver); err != nil {
				rows.Close()
				return err
			}
			list = append(list, r)
		}
		rows.Close()
		for _, r := range list {
			plain, err := s.kr.Decrypt(purpose, r.ct, r.ver)
			if err != nil {
				return err
			}
			ct, ver, err := s.kr.Encrypt(purpose, plain)
			if err != nil {
				return err
			}
			if _, err := q.db.Exec(ctx, `UPDATE `+table+` SET `+col+` = $2, `+verCol+` = $3 WHERE id = $1`, r.id, ct, ver); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

func (s *Store) rekeyUSSD(ctx context.Context, batch int) (int, error) {
	n := 0
	err := s.Tx(ctx, func(q *Q) error {
		rows, err := q.db.Query(ctx, `SELECT provider_name, session_id, last_answer_ciphertext, last_answer_key_version
			FROM ussd_sessions WHERE last_answer_ciphertext IS NOT NULL AND last_answer_key_version <> $1
			LIMIT $2 FOR UPDATE SKIP LOCKED`, s.kr.CurrentVersion(), batch)
		if err != nil {
			return err
		}
		type row struct {
			provider, session string
			ct                []byte
			ver               int
		}
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.provider, &r.session, &r.ct, &r.ver); err != nil {
				rows.Close()
				return err
			}
			list = append(list, r)
		}
		rows.Close()
		for _, r := range list {
			plain, err := s.kr.Decrypt(security.PurposeUSSDAnswer, r.ct, r.ver)
			if err != nil {
				return err
			}
			ct, ver, err := s.kr.Encrypt(security.PurposeUSSDAnswer, plain)
			if err != nil {
				return err
			}
			if _, err := q.db.Exec(ctx, `UPDATE ussd_sessions SET last_answer_ciphertext = $3, last_answer_key_version = $4
				WHERE provider_name = $1 AND session_id = $2`, r.provider, r.session, ct, ver); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}
