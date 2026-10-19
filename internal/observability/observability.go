// Package observability provides structured logging and the Prometheus
// metrics listed in plan §13. Metrics never carry message content or
// phone numbers.
package observability

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

func NewLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

type Metrics struct {
	Registry *prometheus.Registry

	ChannelInbound          *prometheus.CounterVec
	USSDInlineDelivery      prometheus.Counter
	USSDAsyncSMSFallback    prometheus.Counter
	GroundingHits           *prometheus.CounterVec
	GroundingMisses         prometheus.Counter
	SMSInboundCoalesced     prometheus.Counter
	SMSMoreBufferServed     prometheus.Counter
	SMSOutboundSubmitted    *prometheus.CounterVec
	SMSOutboundFailed       *prometheus.CounterVec
	AIRequests              *prometheus.CounterVec
	AIEmptyReasoning        *prometheus.CounterVec
	AIRequestLatency        *prometheus.HistogramVec
	DailySpendUSD           prometheus.Gauge
	DailySpendCapUSD        prometheus.Gauge
	VoucherSegmentsRedeemed prometheus.Counter
	WebhookRejected         *prometheus.CounterVec
	DuplicateInbound        prometheus.Counter
	JobsProcessed           *prometheus.CounterVec
	DailyLimitReached       prometheus.Counter
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		Registry: reg,
		ChannelInbound: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "channel_inbound_total", Help: "Inbound SMS messages and USSD steps.",
		}, []string{"channel", "provider", "carrier"}),
		USSDInlineDelivery: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ussd_inline_delivery_total", Help: "Answers shown inside the USSD session (no SMS cost).",
		}),
		USSDAsyncSMSFallback: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ussd_async_sms_fallback_total", Help: "USSD questions answered by MT SMS after the fast-path deadline.",
		}),
		GroundingHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "grounding_store_hits_total", Help: "Direct answers from the Local Ghana Grounding Store.",
		}, []string{"category"}),
		GroundingMisses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "grounding_store_misses_total", Help: "Questions with no direct grounding answer.",
		}),
		SMSInboundCoalesced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sms_inbound_coalesced_messages_total", Help: "Inbound texts merged into an already pending job.",
		}),
		SMSMoreBufferServed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sms_more_buffer_served_total", Help: "MORE continuations served from the overflow buffer.",
		}),
		SMSOutboundSubmitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sms_outbound_submitted_total", Help: "MT SMS accepted by the provider.",
		}, []string{"provider", "encoding", "segments"}),
		SMSOutboundFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sms_outbound_failed_total", Help: "MT SMS submissions that failed or have an unknown outcome.",
		}, []string{"provider", "reason"}),
		AIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_requests_total", Help: "Model calls by outcome.",
		}, []string{"model", "status"}),
		AIEmptyReasoning: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_empty_reasoning_response_total", Help: "Reasoning-model calls that returned no visible text (must stay 0).",
		}, []string{"model"}),
		AIRequestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ai_request_latency_seconds", Help: "Model call latency.",
			Buckets: []float64{0.25, 0.5, 1, 1.5, 2, 2.5, 3.2, 4, 6, 8, 12, 20},
		}, []string{"model"}),
		DailySpendUSD: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daily_spend_usd_total", Help: "Estimated spend today (SMS segments + AI tokens).",
		}),
		DailySpendCapUSD: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daily_spend_cap_usd", Help: "Configured global daily spend cap.",
		}),
		VoucherSegmentsRedeemed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "voucher_segments_redeemed_total", Help: "Bonus credits granted by voucher redemptions.",
		}),
		WebhookRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_rejected_total", Help: "Webhooks rejected before processing.",
		}, []string{"provider", "reason"}),
		DuplicateInbound: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sms_inbound_duplicate_total", Help: "Repeated inbound webhooks ignored by idempotency.",
		}),
		JobsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jobs_processed_total", Help: "Worker jobs by kind and result.",
		}, []string{"kind", "result"}),
		DailyLimitReached: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "daily_limit_reached_total", Help: "Questions refused because the per-number daily cap was reached.",
		}),
	}
	reg.MustRegister(m.ChannelInbound, m.USSDInlineDelivery, m.USSDAsyncSMSFallback, m.GroundingHits,
		m.GroundingMisses, m.SMSInboundCoalesced, m.SMSMoreBufferServed, m.SMSOutboundSubmitted,
		m.SMSOutboundFailed, m.AIRequests, m.AIEmptyReasoning, m.AIRequestLatency, m.DailySpendUSD,
		m.DailySpendCapUSD, m.VoucherSegmentsRedeemed, m.WebhookRejected, m.DuplicateInbound,
		m.JobsProcessed, m.DailyLimitReached)
	return m
}

// ObserveAICall implements ai.Observer.
func (m *Metrics) ObserveAICall(model, status string, latency time.Duration) {
	m.AIRequests.WithLabelValues(model, status).Inc()
	m.AIRequestLatency.WithLabelValues(model).Observe(latency.Seconds())
}

// ObserveAIEmptyReasoning implements ai.Observer.
func (m *Metrics) ObserveAIEmptyReasoning(model string) {
	m.AIEmptyReasoning.WithLabelValues(model).Inc()
}

func (m *Metrics) ObserveSubmitted(provider, encoding string, segments int) {
	m.SMSOutboundSubmitted.WithLabelValues(provider, encoding, strconv.Itoa(segments)).Inc()
}
