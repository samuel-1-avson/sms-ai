package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mockch "smsaighana/internal/channel/mock"
	"smsaighana/internal/config"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/testenv"
)

func post(t *testing.T, srv *httptest.Server, path, secret string, payload any, tamper bool) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := mockch.SignedRequest(context.Background(), secret, srv.URL+path, body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tamper {
		req.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "photosynthesis", "PHOTOSYNTHESIS", 1)))
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestSignedWebhooksEndToEnd(t *testing.T) {
	env := testenv.New(t, "smsai_it_http", func(c *config.Config) { c.AdminAPIToken = "test-admin-token-0123456789" })
	srv := httptest.NewServer(httpapi.NewHandler(env.App))
	defer srv.Close()
	secret := env.Cfg.MockWebhookSecret

	inbound := mockch.InboundPayload{MessageID: "m-1", From: "+233244123456", To: testenv.ShortCode, Text: "What is photosynthesis?"}

	// Forged and tampered webhooks are rejected before parsing.
	if resp, _ := post(t, srv, "/webhooks/sms/mock/inbound", "wrong-secret", inbound, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged signature: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv, "/webhooks/sms/mock/inbound", secret, inbound, true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv, "/webhooks/sms/nope/inbound", secret, inbound, false); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", resp.StatusCode)
	}

	// A valid webhook is acknowledged fast, and a retry is a duplicate.
	start := time.Now()
	resp, body := post(t, srv, "/webhooks/sms/mock/inbound", secret, inbound, false)
	if resp.StatusCode != http.StatusOK || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("valid webhook: %d in %v: %s", resp.StatusCode, time.Since(start), body)
	}
	if _, body := post(t, srv, "/webhooks/sms/mock/inbound", secret, inbound, false); !strings.Contains(body, `"duplicates":1`) {
		t.Fatalf("retry should be a duplicate: %s", body)
	}
	env.Drain(5 * time.Second)
	sent := env.Sent()
	if len(sent) != 1 {
		t.Fatalf("want one reply, got %d", len(sent))
	}

	// Delivery receipt through the status webhook.
	receipt := mockch.StatusPayload{MessageID: sent[0].ProviderMessageID, Status: "DELIVERED"}
	if resp, body := post(t, srv, "/webhooks/sms/mock/status", secret, receipt, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("status webhook: %d %s", resp.StatusCode, body)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE status = 'delivered'`); n != 1 {
		t.Fatal("receipt not applied")
	}

	// USSD session over HTTP.
	ussd := mockch.USSDPayload{SessionID: "u-1", ServiceCode: testenv.USSDCode, Phone: "0201234567", NewSession: true}
	_, body = post(t, srv, "/webhooks/ussd/mock/session", secret, ussd, false)
	var r mockch.USSDResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.Action != "CON" || !strings.Contains(r.Message, "SMS AI Ghana") {
		t.Fatalf("ussd welcome: %s", body)
	}
	ussd.NewSession, ussd.Input = false, "Explain Ohm's law"
	_, body = post(t, srv, "/webhooks/ussd/mock/session", secret, ussd, false)
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.Action != "CON" || !strings.Contains(r.Message, "V = I x R") {
		t.Fatalf("ussd grounded answer: %s", body)
	}

	// Admin API requires the token and never returns content.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/usage", nil)
	if resp, _ := srv.Client().Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin without token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer test-admin-token-0123456789")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var usage map[string]any
	if err := json.Unmarshal(b, &usage); err != nil || usage["questions"].(float64) != 2 || usage["ussd_inline_answers"].(float64) != 1 {
		t.Fatalf("usage report: %s", b)
	}
	if strings.Contains(string(b), "photosynthesis") {
		t.Fatal("usage report leaked message content")
	}

	// Metrics are exposed.
	resp, err = srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, name := range []string{"channel_inbound_total", "grounding_store_hits_total", "ussd_inline_delivery_total", "sms_inbound_duplicate_total"} {
		if !strings.Contains(string(b), name) {
			t.Errorf("metric %s missing", name)
		}
	}
}
