// Package httpapi exposes the provider webhooks, health/readiness, metrics,
// the admin API and (in development) the channel simulator (plan §9).
package httpapi

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"smsaighana/internal/app"
	"smsaighana/internal/channel"
	"smsaighana/internal/msisdn"
	"smsaighana/internal/repository"
	"smsaighana/internal/security"
)

const maxWebhookBody = 64 << 10

//go:embed simulator.html
var simulatorFS embed.FS

type Server struct {
	a   *app.App
	log *slog.Logger
}

// NewHandler builds the HTTP handler with all routes and middleware.
func NewHandler(a *app.App) http.Handler {
	s := &Server{a: a, log: a.Log}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", s.metricsAuth(promhttp.HandlerFor(a.Metrics.Registry, promhttp.HandlerOpts{})))

	mux.HandleFunc("POST /webhooks/sms/{provider}/inbound", s.smsInbound)
	mux.HandleFunc("POST /webhooks/sms/{provider}/status", s.smsStatus)
	// Some providers (Arkesel) deliver signed receipts as GET ?sms_id=…&status=…
	mux.HandleFunc("GET /webhooks/sms/{provider}/status", s.smsStatus)
	mux.HandleFunc("POST /webhooks/ussd/{provider}/session", s.ussdSession)

	mux.Handle("GET /admin/usage", s.admin(s.usage))
	mux.Handle("GET /admin/status", s.admin(s.status))
	mux.Handle("GET /admin/flags", s.admin(s.getFlags))
	mux.Handle("POST /admin/flags", s.admin(s.setFlags))
	mux.Handle("GET /admin/pools", s.admin(s.listPools))
	mux.Handle("POST /admin/pools", s.admin(s.createPool))
	mux.Handle("POST /admin/pools/{code}/deactivate", s.admin(s.deactivatePool))
	mux.Handle("GET /admin/grounding/search", s.admin(s.searchGrounding))
	mux.Handle("POST /admin/grounding/reload", s.admin(s.reloadGrounding))

	if a.Cfg.IsDevelopment() && a.MockGW != nil {
		mux.Handle("POST /admin/simulate/channel", s.admin(s.simulate))
		mux.Handle("GET /admin/simulate/outbox", s.admin(s.outbox))
		mux.HandleFunc("GET /simulator", s.simulatorPage)
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/simulator", http.StatusFound)
		})
	}
	return s.recoverer(s.logRequests(mux))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests logs method, route, status and latency — never bodies or
// phone numbers.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/admin/simulate/outbox") {
			return
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// admin requires ADMIN_API_TOKEN. Development without a token is open so
// the simulator works out of the box; Validate forbids that elsewhere.
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.a.Cfg.AdminAPIToken
		if token == "" && s.a.Cfg.IsDevelopment() {
			h(w, r)
			return
		}
		if token == "" || !security.ConstantTimeEqual(bearer(r), token) {
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		h(w, r)
	})
}

func (s *Server) metricsAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.a.Cfg.MetricsToken
		if token != "" && !security.ConstantTimeEqual(bearer(r), token) {
			writeError(w, http.StatusUnauthorized, "metrics token required")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.a.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ready",
		"grounding_snippets": s.a.Engine.Matcher.Len(),
		"ai_breaker":         s.a.AI.Breaker().State(),
	})
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
}

func (s *Server) rejectWebhook(w http.ResponseWriter, provider string, err error) {
	switch {
	case errors.Is(err, channel.ErrBadSignature):
		s.a.Metrics.WebhookRejected.WithLabelValues(provider, "signature").Inc()
		s.log.Warn("webhook rejected: bad signature", "provider", provider)
		writeError(w, http.StatusUnauthorized, "invalid signature")
	case errors.Is(err, channel.ErrBadPayload):
		s.a.Metrics.WebhookRejected.WithLabelValues(provider, "payload").Inc()
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("webhook processing failed", "provider", provider, "err", err)
		writeError(w, http.StatusInternalServerError, "temporary failure; please retry")
	}
}

func (s *Server) smsInbound(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := s.a.Registry.SMSInbound(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	events, err := p.ParseInbound(r, body)
	if err != nil {
		s.rejectWebhook(w, name, err)
		return
	}
	duplicates := 0
	for _, ev := range events {
		res, err := s.a.Engine.HandleInboundSMS(r.Context(), ev)
		if err != nil {
			s.rejectWebhook(w, name, err)
			return
		}
		if res.Duplicate {
			duplicates++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "accepted", "messages": len(events), "duplicates": duplicates})
}

func (s *Server) smsStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := s.a.Registry.SMSInbound(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	receipts, err := p.ParseStatus(r, body)
	if err != nil {
		s.rejectWebhook(w, name, err)
		return
	}
	if err := s.a.Engine.HandleDeliveryReceipts(r.Context(), receipts); err != nil {
		s.rejectWebhook(w, name, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "accepted", "receipts": len(receipts)})
}

func (s *Server) ussdSession(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	g, ok := s.a.Registry.USSD(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	req, err := g.ParseSession(r, body)
	if err != nil {
		s.rejectWebhook(w, name, err)
		return
	}
	reply, err := s.a.Engine.HandleUSSD(r.Context(), req)
	if err != nil {
		// Always answer the gateway so the user sees a clean message rather
		// than a carrier timeout.
		s.log.Error("ussd step failed", "provider", name, "err", err)
		reply = channel.USSDReply{Message: "Sorry, something went wrong. Please try again.", End: true}
	}
	if err := g.WriteReply(w, req, reply); err != nil {
		s.log.Error("writing ussd reply", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	day := time.Now().UTC()
	if v := r.URL.Query().Get("day"); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "day must be YYYY-MM-DD")
			return
		}
		day = d
	}
	c := s.a.Cfg
	rep, err := s.a.Store.Q().UsageReport(r.Context(), day, c.SMSSegmentCostUSD, c.GlobalDailySpendCapUSD, c.GlobalDailySMSSegmentCap)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	flags, err := s.a.Store.Q().Flags(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"env": s.a.Cfg.Env, "llm_provider": s.a.Cfg.LLMProvider, "llm_model": s.a.Cfg.LLMModel,
		"llm_fallback_model": s.a.Cfg.LLMFallbackModel, "ai_breaker": s.a.AI.Breaker().State(),
		"flags": flags, "grounding_snippets": s.a.Engine.Matcher.Len(),
		"key_version": s.a.Cfg.CurrentKeyVersion, "insecure_dev_keys": s.a.Cfg.InsecureDevKeys,
	})
}

func (s *Server) getFlags(w http.ResponseWriter, r *http.Request) {
	flags, err := s.a.Store.Q().Flags(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, flags)
}

func (s *Server) setFlags(w http.ResponseWriter, r *http.Request) {
	var in map[string]bool
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || len(in) == 0 {
		writeError(w, http.StatusBadRequest, `body must be like {"ai_enabled": false}`)
		return
	}
	for k, v := range in {
		if err := s.a.Store.Q().SetFlag(r.Context(), k, v); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Warn("kill switch changed", "flag", k, "value", v)
	}
	s.a.Engine.InvalidateFlags()
	s.getFlags(w, r)
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.a.Store.Q().ListPools(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	var p repository.Pool
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	p.VoucherCode = strings.ToUpper(strings.TrimSpace(p.VoucherCode))
	if len(p.VoucherCode) < 3 || len(p.VoucherCode) > 24 || p.SponsorName == "" || p.MaxSegments <= 0 || p.PerUserGrant <= 0 {
		writeError(w, http.StatusBadRequest, "need voucher_code (3-24 chars), sponsor_name, max_segments > 0, per_user_grant > 0")
		return
	}
	created, err := s.a.Store.Q().CreatePool(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusConflict, "could not create pool (duplicate code?)")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) deactivatePool(w http.ResponseWriter, r *http.Request) {
	if err := s.a.Store.Q().SetPoolActive(r.Context(), r.PathValue("code"), false); err != nil {
		writeError(w, http.StatusNotFound, "pool not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}

func (s *Server) searchGrounding(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	hits, err := s.a.Store.Q().SearchSnippets(r.Context(), q, 10)
	if err != nil {
		s.serverError(w, err)
		return
	}
	m := s.a.Engine.Matcher.Match(q)
	resp := map[string]any{"trigram_hits": hits}
	if m.Direct != nil {
		resp["matcher_direct"] = m.Direct.Slug
	}
	var ctxSlugs []string
	for _, c := range m.Context {
		ctxSlugs = append(ctxSlugs, c.Slug)
	}
	resp["matcher_context"] = ctxSlugs
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) reloadGrounding(w http.ResponseWriter, r *http.Request) {
	if err := s.a.Engine.LoadGrounding(r.Context(), nil); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"snippets": s.a.Engine.Matcher.Len()})
}

// ---------------------------------------------------------------------------
// Development simulator
// ---------------------------------------------------------------------------

type simulateRequest struct {
	Channel    string `json:"channel"`
	From       string `json:"from"`
	Body       string `json:"body"`
	SessionID  string `json:"session_id"`
	NewSession bool   `json:"new_session"`
}

func (s *Server) simulate(w http.ResponseWriter, r *http.Request) {
	var in simulateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	switch in.Channel {
	case "sms":
		res, err := s.a.Engine.HandleInboundSMS(r.Context(), channel.InboundSMS{
			Provider: "mock", ProviderEventID: "sim-" + randomID(), ServiceID: s.a.Cfg.SMSServiceID,
			FromE164: in.From, Body: in.Body, ReceivedAt: time.Now(), Environment: "simulator",
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "ussd":
		if in.SessionID == "" {
			writeError(w, http.StatusBadRequest, "session_id is required for ussd")
			return
		}
		reply, err := s.a.Engine.HandleUSSD(r.Context(), channel.USSDRequest{
			Provider: "mock", SessionID: in.SessionID, ServiceCode: s.a.Cfg.USSDServiceCode,
			FromE164: in.From, Input: in.Body, NewSession: in.NewSession,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": reply.Message, "end": reply.End})
	default:
		writeError(w, http.StatusBadRequest, `channel must be "sms" or "ussd"`)
	}
}

func (s *Server) outbox(w http.ResponseWriter, r *http.Request) {
	phone, err := msisdn.Normalize(r.URL.Query().Get("phone"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}
	user, err := s.a.Store.Q().FindUserByPhone(r.Context(), phone)
	if errors.Is(err, repository.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []any{}})
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	msgs, err := s.a.Store.Q().Outbox(r.Context(), user.ID, 40)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "bonus_quota": user.BonusQuota, "opted_out": user.OptedOut})
}

func (s *Server) simulatorPage(w http.ResponseWriter, r *http.Request) {
	b, _ := simulatorFS.ReadFile("simulator.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
