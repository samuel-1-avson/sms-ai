// Package testenv builds a complete application against a throwaway
// PostgreSQL database for integration tests. Tests are skipped unless
// SMSAI_TEST_DATABASE_URL points at a server where the user may create
// databases, e.g. postgres://smsai:smsai_dev_password@localhost:5442/postgres.
package testenv

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"smsaighana/internal/app"
	"smsaighana/internal/channel"
	mockch "smsaighana/internal/channel/mock"
	"smsaighana/internal/config"
	"smsaighana/internal/service"
)

const (
	TestEncryptionKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	TestHMACKey       = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	ShortCode         = "SIM-SHORTCODE"
	USSDCode          = "*713*1#"
)

type Env struct {
	*app.App
	T   testing.TB
	URL string
	seq atomic.Int64
}

// BaseEnviron is the configuration every test starts from: short timers so
// debounce and USSD deadlines can be exercised quickly.
func BaseEnviron(dbURL string) []string {
	return []string{
		"APP_ENV=test", "DATABASE_URL=" + dbURL, "AUTO_MIGRATE=true", "LOG_LEVEL=error",
		"CURRENT_KEY_VERSION=1", "ENCRYPTION_KEYS_V1=" + TestEncryptionKey, "HMAC_KEYS_V1=" + TestHMACKey,
		"INBOUND_DEBOUNCE_MS=300", "INBOUND_COALESCE_EXTEND_MS=300", "USSD_FAST_PATH_TIMEOUT_MS=500",
		"MOCK_LLM_LATENCY_MS=20", "LLM_TIMEOUT_MS=4000", "WORKER_POLL_MS=50",
		"PER_NUMBER_RATE_LIMIT_PER_MINUTE=100", "SMS_SERVICE_ID=" + ShortCode, "USSD_SERVICE_CODE=" + USSDCode,
		"LLM_PROVIDER=mock", "PRIMARY_SMS_PROVIDER=mock", "PRIMARY_USSD_PROVIDER=mock",
	}
}

// FreshDatabase recreates database name on the test server and returns its
// URL. SMSAI_TEST_DB_SUFFIX is appended to the name so two suites (plain and
// -race) can share one server without dropping each other's databases.
func FreshDatabase(t testing.TB, name string) string {
	t.Helper()
	base := os.Getenv("SMSAI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SMSAI_TEST_DATABASE_URL not set; skipping integration test")
	}
	name += os.Getenv("SMSAI_TEST_DB_SUFFIX")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to test server: %v", err)
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatalf("dropping %s: %v", name, err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// New builds an app on a fresh database. mutate may adjust configuration.
func New(t testing.TB, dbName string, mutate func(*config.Config)) *Env {
	t.Helper()
	dbURL := FreshDatabase(t, dbName)
	return NewOn(t, dbURL, BaseEnviron(dbURL), mutate)
}

// NewOn builds an app on an existing database with explicit environment.
func NewOn(t testing.TB, dbURL string, environ []string, mutate func(*config.Config)) *Env {
	t.Helper()
	cfg, err := config.LoadFrom(environ)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	a, err := app.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("building app: %v", err)
	}
	t.Cleanup(a.Close)
	a.MockModel.SlowLatency = 1500 * time.Millisecond
	a.Engine.ReconcileAfter = 0
	return &Env{App: a, T: t, URL: dbURL}
}

// SMS delivers an inbound text as if from the mock provider.
func (e *Env) SMS(from, body string) service.InboundResult {
	e.T.Helper()
	return e.SMSWithID(fmt.Sprintf("evt-%d-%d", time.Now().UnixNano(), e.seq.Add(1)), from, body)
}

func (e *Env) SMSWithID(eventID, from, body string) service.InboundResult {
	e.T.Helper()
	res, err := e.Engine.HandleInboundSMS(context.Background(), channel.InboundSMS{
		Provider: mockch.Name, ProviderEventID: eventID, ServiceID: ShortCode, FromE164: from, Body: body,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		e.T.Fatalf("HandleInboundSMS(%q): %v", body, err)
	}
	return res
}

// USSD sends one USSD step.
func (e *Env) USSD(session, from, input string, newSession bool) channel.USSDReply {
	e.T.Helper()
	reply, err := e.Engine.HandleUSSD(context.Background(), channel.USSDRequest{
		Provider: mockch.Name, SessionID: session, ServiceCode: USSDCode, FromE164: from, Input: input, NewSession: newSession,
	})
	if err != nil {
		e.T.Fatalf("HandleUSSD(%q): %v", input, err)
	}
	return reply
}

// Drain processes jobs until none are pending or retrying, failing the
// test after timeout. Debounced jobs are waited for.
func (e *Env) Drain(timeout time.Duration) {
	e.T.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := e.Store.Q().ClaimJob(ctx, "test-worker", time.Minute)
		if err != nil {
			e.T.Fatalf("claim: %v", err)
		}
		if job != nil {
			if err := e.Engine.ProcessJob(ctx, job); err != nil {
				if rerr := e.Engine.RescheduleAfterError(ctx, job, err); rerr != nil {
					e.T.Fatalf("reschedule: %v", rerr)
				}
			}
			continue
		}
		var open int
		if err := e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs
			WHERE status = 'pending' OR (status = 'running' AND attempts < max_attempts)`).Scan(&open); err != nil {
			e.T.Fatal(err)
		}
		if open == 0 {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	e.T.Fatalf("jobs still open after %v", timeout)
}

// Sent returns MT SMS captured by the mock gateway.
func (e *Env) Sent() []mockch.SentMessage { return e.MockGW.Sent() }

// SentBodies returns just the bodies of captured MT SMS.
func (e *Env) SentBodies() []string {
	var out []string
	for _, m := range e.Sent() {
		out = append(out, m.Body)
	}
	return out
}

// Count runs a count(*) query.
func (e *Env) Count(sql string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Store.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("%s: %v", sql, err)
	}
	return n
}

// KeyEnviron returns environ with the key settings replaced.
func KeyEnviron(environ []string, current int, keys map[int][2]string) []string {
	var out []string
	for _, kv := range environ {
		if strings.HasPrefix(kv, "ENCRYPTION_KEYS_V") || strings.HasPrefix(kv, "HMAC_KEYS_V") || strings.HasPrefix(kv, "CURRENT_KEY_VERSION=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, fmt.Sprintf("CURRENT_KEY_VERSION=%d", current))
	for v, k := range keys {
		out = append(out, fmt.Sprintf("ENCRYPTION_KEYS_V%d=%s", v, k[0]), fmt.Sprintf("HMAC_KEYS_V%d=%s", v, k[1]))
	}
	return out
}

// RandomKey returns a base64 32-byte key derived from a seed byte.
func RandomKey(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(b)
}
