package mnotify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"smsaighana/internal/channel"
)

func TestSendRequestAndResponse(t *testing.T) {
	fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "mnotify_sms_quick_response_openapi_example.json"))
	})
	res, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
	if err != nil {
		t.Fatal(err)
	}
	// The campaign id is the only handle mNotify returns for the send.
	if res.ProviderMessageID != testCampaignID {
		t.Fatalf("provider message id %q", res.ProviderMessageID)
	}
	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("want one call, got %d", len(calls))
	}
	c := calls[0]
	if c.Method != http.MethodPost || c.Path != "/api/sms/quick" {
		t.Fatalf("unexpected %s %s", c.Method, c.Path)
	}
	// The key is the only query parameter, and it arrives intact.
	if len(c.Query) != 1 || c.Query.Get("key") != testKey {
		t.Fatalf("query %v", c.Query)
	}
	if c.Header.Get("Content-Type") != "application/json" || c.Header.Get("Authorization") != "" {
		t.Fatalf("headers %v", c.Header)
	}
	// The body is the documented request, with the number in local form and no
	// sms_type (which would add a charge).
	var got map[string]any
	if err := json.Unmarshal(c.Body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"recipient": []any{"0541234567"}, "sender": "SMS-AI", "message": "Photosynthesis turns light into sugar.",
		"is_schedule": false, "schedule_date": "",
	}
	if !jsonEqual(got, want) {
		t.Fatalf("request body:\n got %v\nwant %v", got, want)
	}
	if _, has := got["sms_type"]; has || strings.Contains(string(c.Body), "otp") {
		t.Fatalf("the request must not ask for an OTP charge: %s", c.Body)
	}
}

func TestSendRequestMatchesTheOpenAPIExampleShape(t *testing.T) {
	var example, ours map[string]any
	if err := json.Unmarshal(fixture(t, "mnotify_sms_quick_request_openapi_example.json"), &example); err != nil {
		t.Fatal(err)
	}
	delete(example, "_note")
	fake := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"_id":"x"}}`))
	if _, err := newTestProvider(fake.base()).Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fake.recorded()[0].Body, &ours); err != nil {
		t.Fatal(err)
	}
	if len(ours) != len(example) {
		t.Fatalf("our request has keys %v, the example %v", keys(ours), keys(example))
	}
	for k := range example {
		if _, ok := ours[k]; !ok {
			t.Errorf("our request lacks %q", k)
		}
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestSendNormalizesTheRecipient(t *testing.T) {
	for _, c := range []struct {
		to, want string
		ok       bool
	}{
		{"+233541234567", "0541234567", true},
		{"233541234567", "0541234567", true},
		{"0541234567", "0541234567", true},
		{" +233201234567 ", "0201234567", true},
		{"+447700900123", "", false}, // not Ghana: the file shows no international recipient
		{"+23354123456", "", false},  // too short
		{"541234567", "", false},
		{"abc", "", false},
		{"", "", false},
	} {
		got, ok := localNumber(c.to)
		if got != c.want || ok != c.ok {
			t.Errorf("localNumber(%q) = %q, %v; want %q, %v", c.to, got, ok, c.want, c.ok)
		}
	}
	fake := newFakeMNotify(t, respond(200, "{}"))
	msg := outbound()
	msg.To = "+447700900123"
	_, err := newTestProvider(fake.base()).Send(context.Background(), msg)
	if channel.SendErrorKindOf(err) != channel.SendPermanent || len(fake.recorded()) != 0 {
		t.Fatalf("a foreign number must be refused before any request: %v (%d calls)", err, len(fake.recorded()))
	}
}

func TestSendRejectsBadMessagesWithoutARequest(t *testing.T) {
	fake := newFakeMNotify(t, respond(200, "{}"))
	p := newTestProvider(fake.base())
	for name, mutate := range map[string]func(*channel.OutboundSMS){
		"no sender":         func(m *channel.OutboundSMS) { m.SenderID = "" },
		"sender too long":   func(m *channel.OutboundSMS) { m.SenderID = "SMS-AI-GHANA" }, // 12 characters
		"empty body":        func(m *channel.OutboundSMS) { m.Body = "" },
		"not a phone":       func(m *channel.OutboundSMS) { m.To = "not-a-number" },
		"short local style": func(m *channel.OutboundSMS) { m.To = "054123" },
	} {
		msg := outbound()
		mutate(&msg)
		if _, err := p.Send(context.Background(), msg); channel.SendErrorKindOf(err) != channel.SendPermanent {
			t.Errorf("%s: want a permanent error, got %v", name, err)
		}
	}
	if n := len(fake.recorded()); n != 0 {
		t.Fatalf("%d requests for invalid messages", n)
	}
	// Exactly 11 characters is fine.
	msg := outbound()
	msg.SenderID = "SMS-AI-GHAN"
	fake2 := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"_id":"x"}}`))
	if _, err := newTestProvider(fake2.base()).Send(context.Background(), msg); err != nil {
		t.Fatalf("an 11-character sender ID: %v", err)
	}
}

// TestSendOutcomes pins how every kind of answer is classified. The OpenAPI
// file prints only the success body, so every failure body below is
// constructed: they are the plain-language errors the classifier is built to
// recognise, plus shapes it must not misread.
func TestSendOutcomes(t *testing.T) {
	const ok = `{"status":"success","code":"2000","message":"messages sent successfully","summary":{"_id":"CAMPAIGN-1","total_sent":1,"total_rejected":0,"credit_left":1482}}`
	for _, c := range []struct {
		name   string
		status int
		body   string
		kind   channel.SendErrorKind
		wantOK bool
		wantID string
	}{
		{"success", 200, ok, 0, true, "CAMPAIGN-1"},
		{"success without an id uses our reference", 200, `{"status":"success","code":"2000"}`, 0, true, testMsgID},
		{"success with a numeric string count", 200, `{"status":"success","code":"2000","summary":{"_id":"C2","total_sent":"1","total_rejected":"0"}}`, 0, true, "C2"},
		{"success with a numeric code", 200, `{"status":"success","code":2000,"summary":{"_id":"C3"}}`, 0, true, "C3"},
		{"success with an upper-case status", 200, `{"status":"SUCCESS","code":"2000","summary":{"_id":"C4"}}`, 0, true, "C4"},
		{"recipient rejected", 200, `{"status":"success","code":"2000","summary":{"_id":"C5","total_sent":0,"total_rejected":1}}`, channel.SendPermanent, false, ""},
		{"zero sent", 200, `{"status":"success","code":"2000","summary":{"_id":"C6","total_sent":0,"total_rejected":0}}`, channel.SendUncertain, false, ""},
		{"success with another code", 200, `{"status":"success","code":"2001","message":"queued"}`, channel.SendUncertain, false, ""},
		{"unreadable body", 200, `<html>maintenance</html>`, channel.SendUncertain, false, ""},
		{"empty body", 200, ``, channel.SendUncertain, false, ""},
		{"body without status", 200, `{"hello":"world"}`, channel.SendUncertain, false, ""},
		{"unregistered sender id", 200, `{"status":"error","code":"4000","message":"Sender ID is not registered"}`, channel.SendPermanent, false, ""},
		{"insufficient balance", 200, `{"status":"error","code":"1002","message":"Insufficient balance"}`, channel.SendPermanent, false, ""},
		{"invalid number", 200, `{"status":"error","message":"Invalid recipient number"}`, channel.SendPermanent, false, ""},
		{"bad key in a 200", 200, `{"status":"error","message":"Invalid API key"}`, channel.SendRetryable, false, ""},
		{"unrecognised error in a 200", 200, `{"status":"error","code":"9999","message":"Something went sideways"}`, channel.SendUncertain, false, ""},
		{"summary is an array in an error", 200, `{"status":"error","message":"Sender not approved","summary":[]}`, channel.SendPermanent, false, ""},

		{"unauthenticated", 401, `{"status":"error","message":"Unauthenticated."}`, channel.SendRetryable, false, ""},
		{"unauthorized without a body", 401, ``, channel.SendRetryable, false, ""},
		{"forbidden", 403, `{"message":"Forbidden"}`, channel.SendRetryable, false, ""},
		{"not found (wrong base URL)", 404, `<html>nginx</html>`, channel.SendRetryable, false, ""},
		{"method not allowed", 405, ``, channel.SendRetryable, false, ""},
		{"too many requests", 429, `{"message":"Too Many Attempts."}`, channel.SendRetryable, false, ""},
		{"request timeout", 408, ``, channel.SendRetryable, false, ""},
		{"payment required", 402, `{"message":"Insufficient balance"}`, channel.SendPermanent, false, ""},
		{"payment required without a body", 402, ``, channel.SendPermanent, false, ""},
		{"validation error", 422, `{"status":"error","message":{"message":["The message field is required."]}}`, channel.SendPermanent, false, ""},
		{"bad request", 400, `{"status":"error","code":"4000","message":"Validation error"}`, channel.SendPermanent, false, ""},
		{"sender rejected in a 400", 400, `{"status":"error","message":"The selected sender is invalid."}`, channel.SendPermanent, false, ""},
		{"balance in a 403", 403, `{"status":"error","message":"Your SMS balance is too low"}`, channel.SendPermanent, false, ""},

		{"server error", 500, `{"message":"Server Error"}`, channel.SendUncertain, false, ""},
		{"bad gateway", 502, `<html>bad gateway</html>`, channel.SendUncertain, false, ""},
		{"service unavailable", 503, ``, channel.SendUncertain, false, ""},
		{"gateway timeout", 504, ``, channel.SendUncertain, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeMNotify(t, respond(c.status, c.body))
			res, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
			if c.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if res.ProviderMessageID != c.wantID {
					t.Fatalf("provider message id %q, want %q", res.ProviderMessageID, c.wantID)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error of kind %d, got success %+v", c.kind, res)
			}
			if got := channel.SendErrorKindOf(err); got != c.kind {
				t.Fatalf("kind %d, want %d: %v", got, c.kind, err)
			}
			if n := len(fake.recorded()); n != 1 {
				t.Fatalf("%d requests; a send must never repeat itself", n)
			}
		})
	}
}

func TestSendErrorTextIsUsefulAndClean(t *testing.T) {
	fake := newFakeMNotify(t, respond(200, `{"status":"error","code":"4000","message":"Sender ID \"SMS-AI\" is not registered; recipient 233541234567 ignored"}`))
	_, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not registered") {
		t.Errorf("mNotify's own words are missing: %v", msg)
	}
	// A phone number mNotify echoed must not reach logs or send_attempts.
	if strings.Contains(msg, "233541234567") || strings.Contains(msg, "0541234567") {
		t.Errorf("phone number in error text: %v", msg)
	}
	// Text from a hostile or broken response is bounded.
	long := strings.Repeat("x", 5000)
	fake2 := newFakeMNotify(t, respond(500, `{"message":"`+long+`"}`))
	_, err = newTestProvider(fake2.base()).Send(context.Background(), outbound())
	if err == nil || len(err.Error()) > 400 {
		t.Errorf("error text not bounded: %d bytes", len(errString(err)))
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSendTransportFailures(t *testing.T) {
	t.Run("connection refused is retryable: nothing left the process", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		base := closed.URL + "/api"
		closed.Close()
		_, err := newTestProvider(base).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable {
			t.Fatalf("kind %d: %v", channel.SendErrorKindOf(err), err)
		}
	})
	t.Run("a cancelled context before sending is retryable", func(t *testing.T) {
		fake := newFakeMNotify(t, respond(200, "{}"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newTestProvider(fake.base()).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable || len(fake.recorded()) != 0 {
			t.Fatalf("kind %d, %d calls: %v", channel.SendErrorKindOf(err), len(fake.recorded()), err)
		}
	})
	t.Run("a timeout after the request left is uncertain", func(t *testing.T) {
		fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := newTestProvider(fake.base()).Send(ctx, outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("kind %d: %v", channel.SendErrorKindOf(err), err)
		}
	})
	t.Run("a connection dropped after the request is uncertain", func(t *testing.T) {
		fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		_, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("kind %d: %v", channel.SendErrorKindOf(err), err)
		}
		if n := len(fake.recorded()); n != 1 {
			t.Fatalf("%d requests", n)
		}
	})
	t.Run("a response cut off mid-body is uncertain", func(t *testing.T) {
		fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"succ`))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		_, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendUncertain {
			t.Fatalf("kind %d: %v", channel.SendErrorKindOf(err), err)
		}
	})
	t.Run("no API key sends nothing and is retryable", func(t *testing.T) {
		fake := newFakeMNotify(t, respond(200, "{}"))
		p := New(Options{BaseURL: fake.base(), Log: quietLog()})
		_, err := p.Send(context.Background(), outbound())
		if channel.SendErrorKindOf(err) != channel.SendRetryable || len(fake.recorded()) != 0 {
			t.Fatalf("kind %d, %d calls: %v", channel.SendErrorKindOf(err), len(fake.recorded()), err)
		}
	})
}

// TestRedirectsAreNotFollowed: the key is in the URL, so a redirect must not
// carry it to another host (or replay a POST as a GET).
func TestRedirectsAreNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","code":"2000","summary":{"_id":"X"}}`))
	}))
	defer other.Close()
	fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/sms/quick?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	})
	_, err := newTestProvider(fake.base()).Send(context.Background(), outbound())
	if channel.SendErrorKindOf(err) != channel.SendRetryable {
		t.Fatalf("a redirect means a wrong base URL and nothing was accepted: kind %d: %v", channel.SendErrorKindOf(err), err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect was followed, with the API key")
	}
}

// TestNotAStatusQuerier pins the decision not to look uncertain sends up: a
// send has no client reference, so there is nothing safe to ask mNotify about,
// and a "not found" would make the reconciler resend a billed message.
func TestNotAStatusQuerier(t *testing.T) {
	var p any = newTestProvider("http://127.0.0.1:1/api")
	if _, ok := p.(channel.StatusQuerier); ok {
		t.Fatal("mnotify must not implement channel.StatusQuerier")
	}
}

func TestInboundSMSAndReportsAreRefused(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	for _, body := range [][]byte{nil, []byte(`{}`), []byte(`{"status":"success","report":[{"_id":1,"status":"DELIVERED"}]}`)} {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/sms/mnotify/status?token="+testUSSDToken, nil)
		if ev, err := p.ParseInbound(r, body); err == nil || !errors.Is(err, channel.ErrBadPayload) || ev != nil {
			t.Errorf("ParseInbound(%s) = %v, %v", body, ev, err)
		}
		if rc, err := p.ParseStatus(r, body); err == nil || !errors.Is(err, channel.ErrBadPayload) || rc != nil {
			t.Errorf("ParseStatus(%s) = %v, %v", body, rc, err)
		}
	}
}

func TestLowCreditWarningIsRateLimited(t *testing.T) {
	log, buf := capturedLog()
	fake := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"_id":"C","total_sent":1,"credit_left":42}}`))
	p := New(Options{APIKey: testKey, BaseURL: fake.base(), Log: log})
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := p.Send(context.Background(), outbound()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "credit is low"); n != 1 {
		t.Fatalf("want one warning, got %d:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "credit_left=42") {
		t.Fatalf("the warning should say how many credits are left:\n%s", buf.String())
	}
	now = now.Add(lowCreditInterval + time.Second)
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "credit is low"); n != 2 {
		t.Fatalf("want a second warning after the interval, got %d", n)
	}
	// Plenty of credit: silence.
	log2, buf2 := capturedLog()
	rich := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"_id":"C","total_sent":1,"credit_left":1483}}`))
	if _, err := New(Options{APIKey: testKey, BaseURL: rich.base(), Log: log2}).Send(context.Background(), outbound()); err != nil || buf2.String() != "" {
		t.Fatalf("unexpected log output or error: %v %q", err, buf2.String())
	}
}
