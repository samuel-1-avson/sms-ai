package mtn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"smsaighana/internal/channel"
)

func TestSendRequestAndResponse(t *testing.T) {
	var got map[string]any
	var header http.Header
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/sms/outbound" || r.URL.RawQuery != "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		}
		header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write(fixture(t, "mtn_sms_send_response_constructed.json"))
	})
	res, err := newTestProvider(fake.URL).Send(context.Background(), outbound())
	if err != nil {
		t.Fatal(err)
	}
	// Store MTN's requestId, independently of our clientCorrelator.
	if res.ProviderMessageID != testRequestID {
		t.Fatalf("provider message id %q", res.ProviderMessageID)
	}
	want := map[string]any{
		"senderAddress": "SMS-AI", "receiverAddress": []any{"233541234567"},
		"message": "Photosynthesis turns light into sugar.", "clientCorrelator": testMsgID,
	}
	if !jsonEqual(got, want) {
		t.Fatalf("request body:\n got %v\nwant %v", got, want)
	}
	if header.Get("Authorization") != "Bearer tok-1" || header.Get("Content-Type") != "application/json" ||
		header.Get("x-api-key") != "" || header.Get("transactionId") != testMsgID {
		t.Fatalf("request headers: %v", header)
	}
	if fake.tokenCalls.Load() != 1 {
		t.Fatalf("token requests: %d", fake.tokenCalls.Load())
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestSendReusesTheTokenAndRefreshesAfter401(t *testing.T) {
	// MTN refuses tok-1 (revoked) and accepts tok-2.
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(fixture(t, "mtn_error_response_swagger_example.json"))
			return
		}
		_, _ = w.Write(fixture(t, "mtn_sms_send_response_constructed.json"))
	})
	p := newTestProvider(fake.URL)
	_, err := p.Send(context.Background(), outbound())
	if channel.SendErrorKindOf(err) != channel.SendRetryable {
		t.Fatalf("first send with a revoked token: %v", err)
	}
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatalf("second send should use a fresh token: %v", err)
	}
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
	if fake.tokenCalls.Load() != 2 {
		t.Fatalf("token requests: %d, want 2 (one per rejected token, none for the cached one)", fake.tokenCalls.Load())
	}
}

func TestSendWithAdditionalAPIKey(t *testing.T) {
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sms-api-key" || r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("SMS credentials: %v", r.Header)
		}
		_, _ = w.Write(fixture(t, "mtn_sms_send_response_constructed.json"))
	})
	p := New(Options{ClientID: testClientID, ClientSecret: testSecret, BaseURL: fake.URL, APIKey: "sms-api-key", SMSSendsAPIKey: true})
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
}

func TestSendErrorKinds(t *testing.T) {
	var followed atomic.Int32
	cases := []struct {
		name   string
		status int
		body   string
		want   channel.SendErrorKind
	}{
		{"401 bad credentials", 401, `{"statusCode":"4000","statusMessage":"Unauthorised"}`, channel.SendRetryable},
		{"402 balance", 402, `{}`, channel.SendRetryable},
		{"403 forbidden", 403, `{"statusCode":"6001","statusMessage":"Insufficient funds"}`, channel.SendRetryable},
		{"404 wrong base URL", 404, `Not Found`, channel.SendRetryable},
		{"405 wrong method or URL", 405, ``, channel.SendRetryable},
		{"407 proxy not authenticated", 407, ``, channel.SendRetryable},
		{"408 request timeout", 408, ``, channel.SendRetryable},
		{"429 rate limited", 429, `{"statusCode":"3006","statusMessage":"Rate exceeded"}`, channel.SendRetryable},
		{"302 is not followed", 302, ``, channel.SendRetryable},
		{"400 bad request", 400, `{"statusCode":"5000","statusMessage":"Invalid Parameters"}`, channel.SendPermanent},
		{"422 validation", 422, `{}`, channel.SendPermanent},
		{"409 conflict", 409, `{}`, channel.SendPermanent},
		{"500 server error", 500, `{"statusCode":"3001","statusMessage":"System error"}`, channel.SendUncertain},
		{"502 from a proxy", 502, `<html>Bad Gateway</html>`, channel.SendUncertain},
		{"503", 503, ``, channel.SendUncertain},
		{"200 unreadable", 200, `<html>ok</html>`, channel.SendUncertain},
		{"200 empty object", 200, `{}`, channel.SendUncertain},
		{"200 error code", 200, `{"statusCode":"3001","statusMessage":"Something odd","data":{"status":"PENDING"}}`, channel.SendUncertain},
		{"200 array body", 200, `[{"statusCode":"0000"}]`, channel.SendUncertain},
		{"200 missing code and request id", 200, `{"data":{}}`, channel.SendUncertain},
	}
	for _, c := range cases {
		fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				followed.Add(1)
				_, _ = w.Write(fixture(t, "mtn_sms_send_response_constructed.json"))
				return
			}
			if c.status == 302 {
				w.Header().Set("Location", "/moved")
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		_, err := newTestProvider(fake.URL).Send(context.Background(), outbound())
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if got := channel.SendErrorKindOf(err); got != c.want {
			t.Errorf("%s: kind %d, want %d (%v)", c.name, got, c.want, err)
		}
		for _, secret := range []string{"541234567", "Photosynthesis", testSecret, testClientID, "tok-1", testDLRToken} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks %q: %v", c.name, secret, err)
			}
		}
	}
	if followed.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestSendAcceptsStatusVariants(t *testing.T) {
	for _, c := range []struct{ body, wantID string }{
		{string(fixture(t, "mtn_sms_send_response_constructed.json")), testRequestID},
		{`{"statusCode":"0000"}`, testMsgID},
		{`{"statusCode":"0000","data":{"requestId":" "}}`, testMsgID},
		{`{"statusCode":"0000","data":{"requestId":"` + strings.Repeat("x", 129) + `"}}`, testMsgID},
		{`{"data":{"requestId":"request-123"}}`, "request-123"},
		{`{"statusCode":"0000","data":{"requestId":123}}`, "123"},
	} {
		fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated) // any 2xx
			_, _ = io.WriteString(w, c.body)
		})
		res, err := newTestProvider(fake.URL).Send(context.Background(), outbound())
		if err != nil || res.ProviderMessageID != c.wantID {
			t.Errorf("%s: %+v %v", c.body, res, err)
		}
	}
}

func TestSendTokenFailureIsRetryableAndSendsNothing(t *testing.T) {
	for name, tokenBody := range map[string]struct {
		status int
		body   string
	}{
		"credentials refused": {401, `{"statusCode":"4000","statusMessage":"Unauthorised"}`},
		"server error":        {500, `oops`},
		"no access_token":     {200, `{"expires_in":"3599"}`},
		"not JSON":            {200, `<html></html>`},
	} {
		var apiCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != DefaultTokenPath {
				apiCalls.Add(1)
				return
			}
			w.WriteHeader(tokenBody.status)
			_, _ = io.WriteString(w, tokenBody.body)
		}))
		_, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
		srv.Close()
		if channel.SendErrorKindOf(err) != channel.SendRetryable || apiCalls.Load() != 0 {
			t.Errorf("%s: kind %d after %d API calls: %v", name, channel.SendErrorKindOf(err), apiCalls.Load(), err)
		}
		if err != nil && (strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), testClientID)) {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
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
		fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newTestProvider(fake.URL).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable || fake.apiCalls.Load() != 0 {
			t.Fatalf("got %v after %d hits", err, fake.apiCalls.Load())
		}
	})
	t.Run("timeout after sending is uncertain", func(t *testing.T) {
		fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err := newTestProvider(fake.URL).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("connection dropped after the request is uncertain", func(t *testing.T) {
		fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		_, err := newTestProvider(fake.URL).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSendValidatesLocally(t *testing.T) {
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {})
	p := newTestProvider(fake.URL)
	cases := map[string]func(*channel.OutboundSMS){
		"empty sender":       func(m *channel.OutboundSMS) { m.SenderID = "" },
		"sender too long":    func(m *channel.OutboundSMS) { m.SenderID = "SMS-AI-GHANA" },
		"bad recipient":      func(m *channel.OutboundSMS) { m.To = "+233 54 123" },
		"recipient is empty": func(m *channel.OutboundSMS) { m.To = "" },
		"empty body":         func(m *channel.OutboundSMS) { m.Body = "" },
		"no client ref":      func(m *channel.OutboundSMS) { m.ClientRef = "" },
		"client ref too long": func(m *channel.OutboundSMS) {
			m.ClientRef = testMsgID + "x" // our client-reference limit
		},
	}
	for name, mutate := range cases {
		msg := outbound()
		mutate(&msg)
		_, err := p.Send(context.Background(), msg)
		var se *channel.SendError
		if !errors.As(err, &se) || se.Kind != channel.SendPermanent {
			t.Errorf("%s: want a permanent error, got %v", name, err)
		}
	}
	if fake.apiCalls.Load() != 0 || fake.tokenCalls.Load() != 0 {
		t.Fatalf("invalid messages reached MTN: %d API calls, %d token calls", fake.apiCalls.Load(), fake.tokenCalls.Load())
	}
	// A recipient without the "+" is accepted as it is.
	msg := outbound()
	msg.To = "233541234567"
	fake2 := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "mtn_sms_send_response_constructed.json"))
	})
	if _, err := newTestProvider(fake2.URL).Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultEndpoints(t *testing.T) {
	a := newAPI(Options{ClientID: "id", ClientSecret: "s"})
	if a.base != "https://api.mtn.com" || a.tokens.tokenURL != "https://api.mtn.com/v1/oauth/access_token" || a.apiKey != "id" {
		t.Fatalf("defaults: base %q token %q key %q", a.base, a.tokens.tokenURL, a.apiKey)
	}
	if got := a.url(smsSendPath); got != "https://api.mtn.com/v1/messages/sms/outbound" {
		t.Fatalf("send URL %q", got)
	}
	b := newAPI(Options{BaseURL: "https://ghana.api.mtn.com/", APIKey: "k", TokenURL: "https://api.mtn.com/v1/oauth/access_token/accesstoken"})
	if b.base != "https://ghana.api.mtn.com" || b.apiKey != "k" || b.tokens.tokenURL != "https://api.mtn.com/v1/oauth/access_token/accesstoken" {
		t.Fatalf("overrides: %+v", b)
	}
	if c := newAPI(Options{BaseURL: "https://sandbox.api.mtn.com"}); c.tokens.tokenURL != "https://sandbox.api.mtn.com/v1/oauth/access_token" {
		t.Fatalf("token URL follows the base URL: %q", c.tokens.tokenURL)
	}
}

func TestTestsCannotReachMTN(t *testing.T) {
	// The guard that keeps test binaries away from MTN's real hosts.
	p := New(Options{ClientID: testClientID, ClientSecret: testSecret}) // default base: api.mtn.com
	_, err := p.Send(context.Background(), outbound())
	if err == nil || !strings.Contains(err.Error(), "refusing to call an MTN host from a test binary") ||
		channel.SendErrorKindOf(err) != channel.SendRetryable {
		t.Fatalf("send to the real host from a test: %v", err)
	}
	if _, err := NewRegistrar(Options{ClientID: "id", ClientSecret: "s"}).SubscribeUSSD(context.Background(), USSDSubscription{}); err == nil ||
		!strings.Contains(err.Error(), "refusing") {
		t.Fatalf("subscribe against the real host from a test: %v", err)
	}
}

func TestParseInboundIsRefused(t *testing.T) {
	_, err := newTestProvider("").ParseInbound(httptest.NewRequest(http.MethodPost, "/webhooks/sms/mtn/inbound", nil), []byte(`{}`))
	if !errors.Is(err, channel.ErrBadPayload) {
		t.Fatalf("got %v", err)
	}
}
