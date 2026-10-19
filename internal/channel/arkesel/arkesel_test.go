package arkesel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/msisdn"
)

const (
	testToken  = "ussd-token-0123456789abcdef0123456789abcdef"
	testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPhone  = "+233544919953"
	docMsgID   = "9b752841-7ee7-4d40-b4fe-768bfb1da4f0"
)

var (
	_ channel.USSDGateway = (*Provider)(nil)
	_ channel.Sender      = (*Provider)(nil)
	_ channel.SMSInbound  = (*Provider)(nil)
)

// fixture reads a payload copied from Arkesel's documentation.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixture_webhooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestProvider(base string) *Provider {
	return New(Options{
		APIKey: "test-api-key", BaseURL: base, CallbackURL: "https://api.example.test/webhooks/sms/arkesel/status",
		Sandbox: true, WebhookSecret: testSecret, USSDTokens: []string{testToken},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func ussdHTTP(token string) *http.Request {
	target := "/webhooks/ussd/arkesel/session"
	if token != "" {
		target += "?" + TokenParam + "=" + url.QueryEscape(token)
	}
	return httptest.NewRequest(http.MethodPost, target, nil)
}

func TestNotAStatusQuerier(t *testing.T) {
	// Arkesel looks messages up only by its own UUID, which uncertain sends
	// never have; a querier would make the reconciler resend them.
	if _, ok := any(New(Options{})).(channel.StatusQuerier); ok {
		t.Fatal("Provider must not implement channel.StatusQuerier (see the package comment)")
	}
}

// ---------------------------------------------------------------------------
// USSD
// ---------------------------------------------------------------------------

func TestParseSessionFromDocs(t *testing.T) {
	p := newTestProvider("")
	first, err := p.ParseSession(ussdHTTP(testToken), fixture(t, "arkesel_ussd_new_session.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := channel.USSDRequest{
		Provider: Name, SessionID: "USSD_DOCUMENTATION:2005506191900168", FromE164: "233271231234",
		Input: "", NewSession: true, Network: "airteltigo",
	}
	if first != want {
		t.Fatalf("new session:\n got %+v\nwant %+v", first, want)
	}
	if e164, err := msisdn.Normalize(first.FromE164); err != nil || e164 != "+233271231234" {
		t.Fatalf("engine cannot normalize msisdn: %q %v", e164, err)
	}

	next, err := p.ParseSession(ussdHTTP(testToken), fixture(t, "arkesel_ussd_continue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID != first.SessionID || next.NewSession || next.Input != "1" {
		t.Fatalf("follow-up step: %+v", next)
	}
}

func TestParseSessionAcceptsNumericFields(t *testing.T) {
	body := `{"sessionID": 2005506191900168, "userID": "U1", "newSession": false, "msisdn": 233271231234, "userData": "Is *this* ok?", "network": "MTN"}`
	req, err := newTestProvider("").ParseSession(ussdHTTP(testToken), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.SessionID != "U1:2005506191900168" || req.FromE164 != "233271231234" || req.Input != "Is *this* ok?" || req.Network != "mtn" {
		t.Fatalf("got %+v", req)
	}
}

func TestParseSessionRejectsBadPayloads(t *testing.T) {
	p := newTestProvider("")
	for name, body := range map[string]string{
		"not json":       `sessionID=1`,
		"no session id":  `{"userID":"U","newSession":true,"msisdn":"233271231234","userData":"*928*1#"}`,
		"no msisdn":      `{"sessionID":"1","userID":"U","newSession":true,"userData":"*928*1#"}`,
		"object as text": `{"sessionID":{"a":1},"msisdn":"233271231234"}`,
	} {
		if _, err := p.ParseSession(ussdHTTP(testToken), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
}

func TestUSSDAuthentication(t *testing.T) {
	body := fixture(t, "arkesel_ussd_new_session.json")
	rotated := New(Options{USSDTokens: []string{"old-token-0123456789abcdefghij", testToken}})
	unconfigured := New(Options{})
	allow := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	direct := New(Options{USSDTokens: []string{testToken}, AllowedIPs: allow})
	proxied := New(Options{USSDTokens: []string{testToken}, AllowedIPs: allow, ClientIPHeader: "X-Forwarded-For"})

	withPeer := func(r *http.Request, addr string) *http.Request { r.RemoteAddr = addr; return r }
	withHeader := func(r *http.Request, v string) *http.Request { r.Header.Set("X-Forwarded-For", v); return r }

	cases := []struct {
		name string
		p    *Provider
		r    *http.Request
		ok   bool
	}{
		{"valid token", newTestProvider(""), ussdHTTP(testToken), true},
		{"missing token", newTestProvider(""), ussdHTTP(""), false},
		{"wrong token", newTestProvider(""), ussdHTTP(testToken + "x"), false},
		{"token in path is not enough", newTestProvider(""), httptest.NewRequest(http.MethodPost, "/webhooks/ussd/arkesel/session/"+testToken, nil), false},
		{"old token during rotation", rotated, ussdHTTP("old-token-0123456789abcdefghij"), true},
		{"new token during rotation", rotated, ussdHTTP(testToken), true},
		{"no token configured", unconfigured, ussdHTTP(""), false},
		{"no token configured, any token", unconfigured, ussdHTTP(testToken), false},
		{"allowlisted peer", direct, withPeer(ussdHTTP(testToken), "203.0.113.7:5555"), true},
		{"allowlisted IPv4-mapped peer", direct, withPeer(ussdHTTP(testToken), "[::ffff:203.0.113.7]:5555"), true},
		{"peer outside allowlist", direct, withPeer(ussdHTTP(testToken), "198.51.100.7:5555"), false},
		{"allowlist needs the token too", direct, withPeer(ussdHTTP(""), "203.0.113.7:5555"), false},
		{"proxy header, right-most hop", proxied, withHeader(withPeer(ussdHTTP(testToken), "10.0.0.2:4000"), "198.51.100.1, 203.0.113.9"), true},
		{"proxy header, forged left hop", proxied, withHeader(withPeer(ussdHTTP(testToken), "10.0.0.2:4000"), "203.0.113.9, 198.51.100.1"), false},
		{"proxy header missing", proxied, withPeer(ussdHTTP(testToken), "203.0.113.7:5555"), false},
	}
	for _, c := range cases {
		_, err := c.p.ParseSession(c.r, body)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", c.name, err)
		}
	}
}

func TestWriteReplyMatchesDocs(t *testing.T) {
	p := newTestProvider("")
	req, err := p.ParseSession(ussdHTTP(testToken), fixture(t, "arkesel_ussd_new_session.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := p.WriteReply(rec, req, channel.USSDReply{Message: "Welcome to ARKESEL USSD Documentation", End: true}); err != nil {
		t.Fatal(err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var got, want map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture(t, "arkesel_ussd_response.json"), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("END reply:\n got %v\nwant %v", got, want)
	}

	rec = httptest.NewRecorder()
	_ = p.WriteReply(rec, req, channel.USSDReply{Message: "Type your question:"})
	var con USSDResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &con); err != nil || !con.ContinueSession || con.Message != "Type your question:" {
		t.Fatalf("CON reply: %s", rec.Body.String())
	}
}

func TestSessionKeyRoundTrip(t *testing.T) {
	for _, userID := range []string{"USSD_DOCUMENTATION", "", "a:b c/%", "app:1:2"} {
		u, s := splitSessionKey(sessionKey(userID, "2005:5506"))
		if u != userID || s != "2005:5506" {
			t.Errorf("%q: got (%q, %q)", userID, u, s)
		}
	}
}

// ---------------------------------------------------------------------------
// MT SMS
// ---------------------------------------------------------------------------

func outbound() channel.OutboundSMS {
	return channel.OutboundSMS{To: testPhone, SenderID: "SMS-AI", Body: "Photosynthesis turns light into sugar.", Encoding: "GSM-7", ClientRef: "msg-1"}
}

func TestSendFromDocs(t *testing.T) {
	var got sendRequest
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/sms/send" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("api-key") != "test-api-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "arkesel_sms_send_response.json"))
	}))
	defer srv.Close()

	res, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderMessageID != docMsgID {
		t.Fatalf("message id %q", res.ProviderMessageID)
	}
	want := sendRequest{
		Sender: "SMS-AI", Message: outbound().Body, Recipients: []string{"233544919953"},
		CallbackURL: "https://api.example.test/webhooks/sms/arkesel/status", Sandbox: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request body:\n got %+v\nwant %+v", got, want)
	}
	if strings.Contains(string(raw), "scheduled_date") || strings.Contains(string(raw), "use_case") {
		t.Fatalf("unexpected fields: %s", raw)
	}
}

func TestSendLiveOmitsSandbox(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write(fixture(t, "arkesel_sms_send_response.json"))
	}))
	defer srv.Close()
	p := New(Options{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sandbox") || strings.Contains(string(raw), "callback_url") {
		t.Fatalf("live send without callback should carry neither field: %s", raw)
	}
}

func TestSendErrorKinds(t *testing.T) {
	var docErrors map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "arkesel_sms_send_errors.json"), &docErrors); err != nil {
		t.Fatal(err)
	}
	var followed atomic.Int32
	cases := []struct {
		name   string
		status int
		body   string
		want   channel.SendErrorKind
	}{
		{"401 bad key (docs)", 401, string(docErrors["401"]), channel.SendRetryable},
		{"402 no balance (docs)", 402, string(docErrors["402"]), channel.SendRetryable},
		{"403 inactive gateway (docs)", 403, string(docErrors["403"]), channel.SendRetryable},
		{"422 validation (docs)", 422, string(docErrors["422"]), channel.SendPermanent},
		{"500 request failed (docs)", 500, string(docErrors["500"]), channel.SendUncertain},
		{"502 from a proxy", 502, `<html>Bad Gateway</html>`, channel.SendUncertain},
		{"400 bad request", 400, `{}`, channel.SendPermanent},
		{"404 wrong base URL", 404, `Not Found`, channel.SendRetryable},
		{"429 rate limited", 429, ``, channel.SendRetryable},
		{"302 is not followed", 302, ``, channel.SendRetryable},
		{"200 number listed invalid", 200, `{"status":"success","data":[{"invalid numbers":["233544919953"]}]}`, channel.SendPermanent},
		{"200 unreadable", 200, `<html>ok</html>`, channel.SendUncertain},
		{"200 without id", 200, `{"status":"success","data":[]}`, channel.SendUncertain},
		{"200 with error status", 200, `{"status":"error","message":"Something odd"}`, channel.SendUncertain},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				followed.Add(1)
				_, _ = w.Write(fixture(t, "arkesel_sms_send_response.json"))
				return
			}
			if c.status == 302 {
				w.Header().Set("Location", "/moved")
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		_, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
		srv.Close()
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if got := channel.SendErrorKindOf(err); got != c.want {
			t.Errorf("%s: kind %d, want %d (%v)", c.name, got, c.want, err)
		}
		if strings.Contains(err.Error(), "544919953") || strings.Contains(err.Error(), "Photosynthesis") {
			t.Errorf("%s: error leaks the request: %v", c.name, err)
		}
	}
	if followed.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestSendTransportFailures(t *testing.T) {
	t.Run("connection refused is retryable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		_, err := newTestProvider(base).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("cancelled before sending is retryable", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newTestProvider(srv.URL).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable || hits.Load() != 0 {
			t.Fatalf("got %v after %d hits", err, hits.Load())
		}
	})
	t.Run("timeout after sending is uncertain", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := newTestProvider(srv.URL).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("connection dropped after the request is uncertain", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}))
		defer srv.Close()
		_, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSendValidatesLocally(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	p := newTestProvider(srv.URL)
	for name, mutate := range map[string]func(*channel.OutboundSMS){
		"sender too long": func(m *channel.OutboundSMS) { m.SenderID = "SMS-AI-GHANA" },
		"no sender":       func(m *channel.OutboundSMS) { m.SenderID = "" },
		"bad recipient":   func(m *channel.OutboundSMS) { m.To = "+233 54 491" },
		"empty body":      func(m *channel.OutboundSMS) { m.Body = "" },
	} {
		msg := outbound()
		mutate(&msg)
		if _, err := p.Send(context.Background(), msg); channel.SendErrorKindOf(err) != channel.SendPermanent {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid messages reached Arkesel %d times", hits.Load())
	}
	msg := outbound()
	msg.SenderID = "SMS AI GH 1" // 11 characters including spaces is allowed
	_, _ = p.Send(context.Background(), msg)
	if hits.Load() != 1 {
		t.Fatal("11-character sender ID was rejected locally")
	}
}

func TestLookupFromDocs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("api-key") != "test-api-key" {
			t.Errorf("unexpected %s with headers %v", r.Method, r.Header)
		}
		if r.URL.Path != "/api/v2/sms/f3be70c1-3545-4677-b607-6b5f32202652" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(fixture(t, "arkesel_sms_details_response.json"))
	}))
	defer srv.Close()
	p := newTestProvider(srv.URL)

	rec, found, err := p.Lookup(context.Background(), "f3be70c1-3545-4677-b607-6b5f32202652")
	if err != nil || !found || rec.Status != "delivered" || rec.Provider != Name || rec.ProviderMessageID != "f3be70c1-3545-4677-b607-6b5f32202652" {
		t.Fatalf("lookup: %+v found=%v err=%v", rec, found, err)
	}
	if _, found, err := p.Lookup(context.Background(), "b35e5b16-c66c-4561-b0ab-2f6ae50c8757"); err != nil || found {
		t.Fatalf("unknown id: found=%v err=%v", found, err)
	}
	if _, _, err := p.Lookup(context.Background(), ""); err == nil {
		t.Fatal("empty id must be an error, never 'not found'")
	}
}

// ---------------------------------------------------------------------------
// Delivery reports
// ---------------------------------------------------------------------------

type callbackFixture struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     map[string]string `json:"query"`
	Timestamp string            `json:"timestamp"`
	Canonical string            `json:"canonical"`
}

func loadCallback(t *testing.T) callbackFixture {
	t.Helper()
	var fx callbackFixture
	if err := json.Unmarshal(fixture(t, "arkesel_sms_status_callback.json"), &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func callbackRequest(method, path string, query map[string]string, ts, signature string) *http.Request {
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	r := httptest.NewRequest(method, path+"?"+q.Encode(), nil)
	if ts != "" {
		r.Header.Set(HeaderTimestamp, ts)
	}
	if signature != "" {
		r.Header.Set(HeaderSignature, signature)
	}
	r.Header.Set(HeaderWebhookID, "8a3a9f1d-4f3e-46a1-93d3-c9b9e0a6f86f")
	return r
}

func providerAt(secret string, ts string) *Provider {
	p := newTestProvider("")
	p.opt.WebhookSecret = secret
	sec, _ := strconv.ParseInt(ts, 10, 64)
	p.now = func() time.Time { return time.Unix(sec, 0).Add(30 * time.Second) }
	return p
}

func TestCanonicalStringMatchesDocs(t *testing.T) {
	fx := loadCallback(t)
	payload := map[string]any{}
	for k, v := range fx.Query {
		payload[k] = v
	}
	c, err := CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.Timestamp + "." + c; got != fx.Canonical {
		t.Fatalf("canonical string\n got %s\nwant %s", got, fx.Canonical)
	}
}

func TestParseStatusFromDocs(t *testing.T) {
	fx := loadCallback(t)
	// Signed independently of this package's encoder: HMAC over the
	// canonical string exactly as printed in Arkesel's docs.
	sig := "v1=" + hmacHex(testSecret, fx.Canonical)
	p := providerAt(testSecret, fx.Timestamp)
	for _, method := range []string{fx.Method, http.MethodPost} {
		receipts, err := p.ParseStatus(callbackRequest(method, fx.Path, fx.Query, fx.Timestamp, sig), nil)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if len(receipts) != 1 {
			t.Fatalf("%s: %d receipts", method, len(receipts))
		}
		r := receipts[0]
		if r.Provider != Name || r.ProviderMessageID != "abc123" || r.Status != "delivered" || r.ErrorCode != "" || !r.At.Equal(time.Unix(1783425600, 0)) {
			t.Fatalf("%s: %+v", method, r)
		}
	}
}

func TestParseStatusRejectsUnauthenticated(t *testing.T) {
	fx := loadCallback(t)
	good := "v1=" + hmacHex(testSecret, fx.Canonical)
	other := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	tampered := map[string]string{"sms_id": "abc123", "status": "NOT_DELIVERED"}
	ts, _ := strconv.ParseInt(fx.Timestamp, 10, 64)

	cases := []struct {
		name string
		p    *Provider
		r    *http.Request
	}{
		{"no headers", providerAt(testSecret, fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, "", "")},
		{"no signature", providerAt(testSecret, fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, "")},
		{"wrong secret", providerAt(other, fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, good)},
		{"tampered status", providerAt(testSecret, fx.Timestamp), callbackRequest("GET", fx.Path, tampered, fx.Timestamp, good)},
		{"bare hex without v1=", providerAt(testSecret, fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, strings.TrimPrefix(good, "v1="))},
		{"stale timestamp", providerAt(testSecret, strconv.FormatInt(ts+6*60, 10)), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, good)},
		{"future timestamp", providerAt(testSecret, strconv.FormatInt(ts-6*60, 10)), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, good)},
		{"malformed timestamp", providerAt(testSecret, fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, "+"+fx.Timestamp, good)},
		{"secret not configured", providerAt("", fx.Timestamp), callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, good)},
	}
	for _, c := range cases {
		if _, err := c.p.ParseStatus(c.r, nil); !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", c.name, err)
		}
	}
}

func TestParseStatusDuringSecretRotation(t *testing.T) {
	fx := loadCallback(t)
	oldSecret, newSecret := "old-secret-0123456789", testSecret
	header := "v1=" + hmacHex(newSecret, fx.Canonical) + ", v1=" + hmacHex(oldSecret, fx.Canonical)
	for _, secret := range []string{oldSecret, newSecret} {
		if _, err := providerAt(secret, fx.Timestamp).ParseStatus(callbackRequest("GET", fx.Path, fx.Query, fx.Timestamp, header), nil); err != nil {
			t.Errorf("secret %q: %v", secret, err)
		}
	}
}

func TestParseStatusPayloadErrors(t *testing.T) {
	fx := loadCallback(t)
	p := providerAt(testSecret, fx.Timestamp)
	ts, _ := strconv.ParseInt(fx.Timestamp, 10, 64)

	onlyID := map[string]string{"sms_id": "abc123"}
	sig, err := Sign(testSecret, ts, map[string]string{"sms_id": "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ParseStatus(callbackRequest("GET", fx.Path, onlyID, fx.Timestamp, sig), nil); !errors.Is(err, channel.ErrBadPayload) {
		t.Errorf("signed but no status: %v", err)
	}
	empty := httptest.NewRequest(http.MethodPost, fx.Path, nil)
	if _, err := p.ParseStatus(empty, nil); !errors.Is(err, channel.ErrBadPayload) {
		t.Errorf("empty callback: %v", err)
	}
}

func TestParseStatusJSONBodyFallback(t *testing.T) {
	fx := loadCallback(t)
	p := providerAt(testSecret, fx.Timestamp)
	body := []byte(`{"status": "NOT_DELIVERED", "sms_id": "abc123"}`)
	ts, _ := strconv.ParseInt(fx.Timestamp, 10, 64)
	sig, err := Sign(testSecret, ts, map[string]any{"sms_id": "abc123", "status": "NOT_DELIVERED"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, fx.Path, bytes.NewReader(body))
	r.Header.Set(HeaderTimestamp, fx.Timestamp)
	r.Header.Set(HeaderSignature, sig)
	receipts, err := p.ParseStatus(r, body)
	if err != nil || len(receipts) != 1 || receipts[0].Status != "failed" || receipts[0].ErrorCode != "NOT_DELIVERED" {
		t.Fatalf("receipts %+v err %v", receipts, err)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[string][2]string{
		"DELIVERED":     {"delivered", ""},
		"delivered":     {"delivered", ""},
		"SUBMITTED":     {"submitted", ""},
		"QUEUED":        {"submitted", ""},
		"NOT_DELIVERED": {"failed", "NOT_DELIVERED"},
		"Not Delivered": {"failed", "NOT_DELIVERED"},
		"PROHIBITED":    {"rejected", "PROHIBITED"},
		"EXPIRED":       {"expired", "EXPIRED"},
		"SOMETHING_NEW": {"unknown", "SOMETHING_NEW"},
	}
	for raw, want := range cases {
		if st, code := mapStatus(raw); st != want[0] || code != want[1] {
			t.Errorf("%q: got (%q, %q), want %v", raw, st, code, want)
		}
	}
}

func TestParseInboundIsRefused(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/sms/arkesel/inbound", nil)
	if _, err := newTestProvider("").ParseInbound(r, []byte(`{}`)); !errors.Is(err, channel.ErrBadPayload) {
		t.Fatalf("got %v", err)
	}
}

// TestCanonicalJSONMatchesPHP pins json_encode(..., JSON_UNESCAPED_SLASHES)
// behaviour: sorted keys, kept list order, unescaped "/", "<", ">", "&",
// non-ASCII as UTF-16 \u escapes and PHP's empty-array quirk.
func TestCanonicalJSONMatchesPHP(t *testing.T) {
	const bs = "\\"
	in := map[string]any{
		"z": "caf" + string(rune(0xe9)) + " / <b>&amp;",
		"a": []any{"2", "1", map[string]any{"y": "1", "x": json.Number("2")}},
		"m": "line1\nline2\t" + string(rune(0x1f600)) + `"q"` + bs,
		"c": string(rune(0x01)) + string(rune(0x7f)),
		"e": map[string]any{},
		"b": true,
		"n": nil,
	}
	want := `{"a":["2","1",{"x":2,"y":"1"}],"b":true,"c":"` + bs + "u0001" + string(rune(0x7f)) + `","e":[],` +
		`"m":"line1` + bs + `nline2` + bs + `t` + bs + "ud83d" + bs + "ude00" + bs + `"q` + bs + `"` + bs + bs + `",` +
		`"n":null,"z":"caf` + bs + "u00e9" + ` / <b>&amp;"}`
	got, err := CanonicalJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if _, err := CanonicalJSON(map[string]any{"k": string([]byte{0xff})}); err == nil {
		t.Fatal("invalid UTF-8 must not be signed")
	}
	if _, err := CanonicalJSON(map[string]any{"k": 1.5}); err == nil {
		t.Fatal("float64 is not a supported value")
	}
}
