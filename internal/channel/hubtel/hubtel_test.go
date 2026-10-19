package hubtel

import (
	"context"
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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/msisdn"
)

const (
	testUSSDToken = "ussd-token-0123456789abcdef0123456789abcdef"
	testDLRToken  = "dlr-token-0123456789abcdef0123456789abcdef"
	testPhone     = "+233544919953"
	docMsgID      = "f2665231-522f-32b6-accf-6ac8426bfd5c"
)

var (
	_ channel.USSDGateway = (*Provider)(nil)
	_ channel.Sender      = (*Provider)(nil)
	_ channel.SMSInbound  = (*Provider)(nil)
)

// fixture reads a payload from tests/fixture_webhooks. Their provenance is
// listed in docs/PROVIDER_VALIDATION.md: the USSD requests are copied from
// Hubtel's documentation and SDK, the Release/Timeout and delivery-report
// ones are constructed or assumed and say so in their names.
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
		ClientID: "test-client-id", ClientSecret: "test-client-secret", BaseURL: base,
		USSDTokens: []string{testUSSDToken}, DLRTokens: []string{testDLRToken},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func callbackHTTP(method, path, token string, body io.Reader) *http.Request {
	if token != "" {
		path += "?" + TokenParam + "=" + url.QueryEscape(token)
	}
	return httptest.NewRequest(method, path, body)
}

func ussdHTTP(token string) *http.Request {
	return callbackHTTP(http.MethodPost, "/webhooks/ussd/hubtel/session", token, nil)
}

func dlrHTTP(token string, body string) *http.Request {
	return callbackHTTP(http.MethodPost, "/webhooks/sms/hubtel/status", token, strings.NewReader(body))
}

func TestNotAStatusQuerier(t *testing.T) {
	// Hubtel is not known to offer a lookup by our client reference, and a
	// lookup by its own id cannot find a send whose response was lost.
	if _, ok := any(New(Options{})).(channel.StatusQuerier); ok {
		t.Fatal("Provider must not implement channel.StatusQuerier (see the package comment)")
	}
}

// ---------------------------------------------------------------------------
// USSD
// ---------------------------------------------------------------------------

func TestParseSessionFromDocs(t *testing.T) {
	p := newTestProvider("")
	// Samples 1 and 2 of Hubtel's "Building a Programmable Service" page.
	first, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "hubtel_ussd_initiation.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := channel.USSDRequest{
		Provider: Name, SessionID: "3c796dac28174f739de4262d08409c51", FromE164: "233208074263",
		Input: "", NewSession: true, Network: "vodafone",
	}
	if first != want {
		t.Fatalf("initiation:\n got %+v\nwant %+v", first, want)
	}
	if e164, err := msisdn.Normalize(first.FromE164); err != nil || e164 != "+233208074263" {
		t.Fatalf("engine cannot normalize mobile: %q %v", e164, err)
	}

	next, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "hubtel_ussd_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID != first.SessionID || next.NewSession || next.Input != "1" || next.FromE164 != first.FromE164 {
		t.Fatalf("follow-up step: %+v", next)
	}
}

func TestParseSessionFromSDKSample(t *testing.T) {
	// The SDK README uses PascalCase keys and the dialled code as the message
	// on initiation; it is not a menu entry.
	req, err := newTestProvider("").ParseSession(ussdHTTP(testUSSDToken), fixture(t, "hubtel_ussd_initiation_sdk.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := channel.USSDRequest{
		Provider: Name, SessionID: "234567ityjhgwe654765utrye", FromE164: "233244234546",
		Input: "", NewSession: true, Network: "mtn",
	}
	if req != want {
		t.Fatalf("got %+v want %+v", req, want)
	}
}

func TestParseSessionKeepsLatestInputOnly(t *testing.T) {
	// Message is the latest entry; a "*" inside a question stays intact.
	body := `{"platform":"USSD","operator":"MTN","mobile":233544919953,"message":"Is 2*3 equal to 6?","sessionId":"abc","type":"RESPONSE"}`
	req, err := newTestProvider("").ParseSession(ussdHTTP(testUSSDToken), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Input != "Is 2*3 equal to 6?" || req.NewSession || req.FromE164 != "233544919953" || req.Network != "mtn" {
		t.Fatalf("got %+v", req)
	}
}

func TestReleaseAndTimeoutEndTheSession(t *testing.T) {
	p := newTestProvider("")
	for _, name := range []string{"hubtel_ussd_release_constructed.json", "hubtel_ussd_timeout_constructed.json"} {
		req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Delivered to the engine as the "0.Exit" entry, never as a question
		// or a new session.
		if req.Input != exitInput || req.NewSession || req.SessionID != "3c796dac28174f739de4262d08409c51" {
			t.Errorf("%s: got %+v", name, req)
		}
	}
}

func TestParseSessionRejectsBadPayloads(t *testing.T) {
	p := newTestProvider("")
	for name, body := range map[string]string{
		"not json":         `sessionId=1`,
		"no session id":    `{"type":"initiation","mobile":"233208074263","platform":"USSD"}`,
		"no mobile":        `{"type":"initiation","sessionId":"1","platform":"USSD"}`,
		"object as text":   `{"type":"initiation","sessionId":{"a":1},"mobile":"233208074263"}`,
		"unknown type":     `{"type":"banana","sessionId":"1","mobile":"233208074263"}`,
		"missing type":     `{"sessionId":"1","mobile":"233208074263"}`,
		"query type":       `{"type":"Query","sessionId":"1","mobile":"233208074263","platform":"USSD"}`,
		"favorite type":    `{"type":"Favorite","sessionId":"1","mobile":"233208074263","platform":"USSD"}`,
		"hubtel app":       `{"type":"initiation","sessionId":"1","mobile":"233208074263","platform":"Hubtel-App"}`,
		"webstore":         `{"type":"Response","sessionId":"1","mobile":"233208074263","platform":"Webstore"}`,
		"empty body":       ``,
		"array not object": `[{"type":"initiation"}]`,
	} {
		if _, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
	// A missing platform is accepted: Hubtel's SDK falls back to the operator
	// name to recognise USSD.
	if _, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(`{"type":"initiation","sessionId":"1","mobile":"233208074263","operator":"mtn"}`)); err != nil {
		t.Errorf("no platform: %v", err)
	}
}

func TestUSSDAuthentication(t *testing.T) {
	body := fixture(t, "hubtel_ussd_initiation.json")
	rotated := New(Options{USSDTokens: []string{"old-token-0123456789abcdefghij", testUSSDToken}})
	unconfigured := New(Options{})
	onlyDLR := New(Options{DLRTokens: []string{testDLRToken}})
	allow := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	direct := New(Options{USSDTokens: []string{testUSSDToken}, USSDAllowedIPs: allow})
	proxied := New(Options{USSDTokens: []string{testUSSDToken}, USSDAllowedIPs: allow, ClientIPHeader: "X-Forwarded-For"})

	withPeer := func(r *http.Request, addr string) *http.Request { r.RemoteAddr = addr; return r }
	withHeader := func(r *http.Request, v string) *http.Request { r.Header.Set("X-Forwarded-For", v); return r }

	cases := []struct {
		name string
		p    *Provider
		r    *http.Request
		ok   bool
	}{
		{"valid token", newTestProvider(""), ussdHTTP(testUSSDToken), true},
		{"missing token", newTestProvider(""), ussdHTTP(""), false},
		{"wrong token", newTestProvider(""), ussdHTTP(testUSSDToken + "x"), false},
		{"prefix of the token", newTestProvider(""), ussdHTTP(testUSSDToken[:20]), false},
		{"delivery-report token does not open USSD", newTestProvider(""), ussdHTTP(testDLRToken), false},
		{"token in path is not enough", newTestProvider(""), httptest.NewRequest(http.MethodPost, "/webhooks/ussd/hubtel/session/"+testUSSDToken, nil), false},
		{"token in a header is not enough", newTestProvider(""), func() *http.Request {
			r := ussdHTTP("")
			r.Header.Set("Authorization", "Bearer "+testUSSDToken)
			return r
		}(), false},
		{"old token during rotation", rotated, ussdHTTP("old-token-0123456789abcdefghij"), true},
		{"new token during rotation", rotated, ussdHTTP(testUSSDToken), true},
		{"no USSD token configured", unconfigured, ussdHTTP(""), false},
		{"no USSD token configured, any token", unconfigured, ussdHTTP(testUSSDToken), false},
		{"only a DLR token configured", onlyDLR, ussdHTTP(testDLRToken), false},
		{"allowlisted peer", direct, withPeer(ussdHTTP(testUSSDToken), "203.0.113.7:5555"), true},
		{"allowlisted IPv4-mapped peer", direct, withPeer(ussdHTTP(testUSSDToken), "[::ffff:203.0.113.7]:5555"), true},
		{"peer outside allowlist", direct, withPeer(ussdHTTP(testUSSDToken), "198.51.100.7:5555"), false},
		{"allowlist needs the token too", direct, withPeer(ussdHTTP(""), "203.0.113.7:5555"), false},
		{"proxy header, right-most hop", proxied, withHeader(withPeer(ussdHTTP(testUSSDToken), "10.0.0.2:4000"), "198.51.100.1, 203.0.113.9"), true},
		{"proxy header, forged left hop", proxied, withHeader(withPeer(ussdHTTP(testUSSDToken), "10.0.0.2:4000"), "203.0.113.9, 198.51.100.1"), false},
		{"proxy header missing", proxied, withPeer(ussdHTTP(testUSSDToken), "203.0.113.7:5555"), false},
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
	// Authentication comes before parsing: a bad token with garbage is 401, not 400.
	if _, err := newTestProvider("").ParseSession(ussdHTTP(""), []byte("garbage")); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("unauthenticated garbage: %v", err)
	}
}

func TestWriteReply(t *testing.T) {
	p := newTestProvider("")
	req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "hubtel_ussd_initiation.json"))
	if err != nil {
		t.Fatal(err)
	}

	write := func(reply channel.USSDReply) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		if err := p.WriteReply(rec, req, reply); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("not JSON: %s", rec.Body.String())
		}
		return rec, got
	}

	rec, con := write(channel.USSDReply{Message: "SMS AI Ghana\nType your question:\n1.Help 2.New chat"})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	wantCon := map[string]any{
		"type": "Response", "message": "SMS AI Ghana\nType your question:\n1.Help 2.New chat",
		"SessionId": "3c796dac28174f739de4262d08409c51", "clientState": nil, "label": "SMS AI Ghana",
		"dataType": "input", "fieldType": "text", "fieldName": "", "item": nil, "data": nil,
	}
	if !reflect.DeepEqual(con, wantCon) {
		t.Fatalf("continue reply:\n got %v\nwant %v", con, wantCon)
	}

	_, end := write(channel.USSDReply{Message: "Thanks! <Bye> & see you", End: true})
	if end["type"] != "Release" || end["dataType"] != "display" || end["SessionId"] != "3c796dac28174f739de4262d08409c51" ||
		end["message"] != "Thanks! <Bye> & see you" {
		t.Fatalf("end reply: %v", end)
	}
	// Every field Hubtel marks mandatory is present on both replies.
	for _, m := range []map[string]any{con, end} {
		for _, k := range []string{"SessionId", "type", "message", "label", "dataType", "fieldType"} {
			if s, _ := m[k].(string); s == "" {
				t.Errorf("mandatory field %q is empty in %v", k, m)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// MT SMS
// ---------------------------------------------------------------------------

func outbound() channel.OutboundSMS {
	return channel.OutboundSMS{To: testPhone, SenderID: "SMS-AI", Body: "Photosynthesis turns light into sugar.", Encoding: "GSM-7", ClientRef: "msg-1"}
}

func TestSendRequestAndResponse(t *testing.T) {
	var got sendRequest
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" || r.URL.RawQuery != "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "test-client-id" || pass != "test-client-secret" {
			t.Errorf("basic auth: %q %q %v", user, pass, ok)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture(t, "hubtel_sms_send_response.json"))
	}))
	defer srv.Close()

	res, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderMessageID != docMsgID {
		t.Fatalf("message id %q", res.ProviderMessageID)
	}
	want := sendRequest{From: "SMS-AI", To: "233544919953", Content: outbound().Body, RegisteredDelivery: true, ClientReference: "msg-1"}
	if got != want {
		t.Fatalf("request body:\n got %+v\nwant %+v", got, want)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	for _, k := range []string{"From", "To", "Content", "RegisteredDelivery", "ClientReference"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("request body lacks %q: %s", k, raw)
		}
	}
	if len(keys) != 5 {
		t.Errorf("unexpected fields: %s", raw)
	}
}

func TestSendAcceptsStatus200AndWrappedIDs(t *testing.T) {
	for name, body := range map[string]string{
		"top level":        `{"Status":0,"MessageId":"m-1","Rate":1}`,
		"lower case keys":  `{"status":0,"messageId":"m-1"}`,
		"data object":      `{"Status":0,"Data":{"MessageId":"m-1"}}`,
		"data list":        `{"Data":[{"MessageId":"m-1"}]}`,
		"numeric id":       `{"MessageId":1}`,
		"with other noise": `{"Status":0,"MessageId":"m-1","Rate":1.5,"NetworkId":"62002","ClientReference":"msg-1","Detail":null}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		res, err := newTestProvider(srv.URL).Send(context.Background(), outbound())
		srv.Close()
		wantID := "m-1"
		if name == "numeric id" {
			wantID = "1"
		}
		if err != nil || res.ProviderMessageID != wantID {
			t.Errorf("%s: got %+v %v", name, res, err)
		}
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
		{"401 bad credentials", 401, `{"Message":"Unauthorized"}`, channel.SendRetryable},
		{"402 no balance", 402, `{}`, channel.SendRetryable},
		{"403 forbidden", 403, `{"message":"Forbidden"}`, channel.SendRetryable},
		{"404 wrong base URL", 404, `Not Found`, channel.SendRetryable},
		{"405 wrong method or URL", 405, ``, channel.SendRetryable},
		{"408 request timeout", 408, ``, channel.SendRetryable},
		{"429 rate limited", 429, ``, channel.SendRetryable},
		{"302 is not followed", 302, ``, channel.SendRetryable},
		{"400 bad request", 400, `{"Message":"Invalid sender"}`, channel.SendPermanent},
		{"422 validation", 422, `{}`, channel.SendPermanent},
		{"409 conflict", 409, `{}`, channel.SendPermanent},
		{"500 server error", 500, `{"Message":"boom"}`, channel.SendUncertain},
		{"502 from a proxy", 502, `<html>Bad Gateway</html>`, channel.SendUncertain},
		{"503", 503, ``, channel.SendUncertain},
		{"200 unreadable", 200, `<html>ok</html>`, channel.SendUncertain},
		{"200 empty object", 200, `{}`, channel.SendUncertain},
		{"200 error status without id", 200, `{"Status":1,"Message":"Something odd"}`, channel.SendUncertain},
		{"200 empty id", 200, `{"MessageId":""}`, channel.SendUncertain},
		{"200 array body", 200, `[{"MessageId":"m-1"}]`, channel.SendUncertain},
		{"201 without id", 201, `{"Status":0}`, channel.SendUncertain},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				followed.Add(1)
				_, _ = w.Write(fixture(t, "hubtel_sms_send_response.json"))
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
		for _, secret := range []string{"544919953", "Photosynthesis", "test-client-secret", "test-client-id"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks %q: %v", c.name, secret, err)
			}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(fixture(t, "hubtel_sms_send_response.json"))
	}))
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
		t.Fatalf("invalid messages reached Hubtel %d times", hits.Load())
	}
	msg := outbound()
	msg.SenderID = "SMS AI GH 1" // 11 characters including spaces is allowed
	if _, err := p.Send(context.Background(), msg); err != nil || hits.Load() != 1 {
		t.Fatalf("11-character sender ID: %v after %d hits", err, hits.Load())
	}
}

func TestSendOmitsEmptyClientReference(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write(fixture(t, "hubtel_sms_send_response.json"))
	}))
	defer srv.Close()
	msg := outbound()
	msg.ClientRef = ""
	if _, err := newTestProvider(srv.URL).Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ClientReference") {
		t.Fatalf("empty client reference was sent: %s", raw)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if p := New(Options{}); p.base != DefaultBaseURL {
		t.Fatalf("base %q", p.base)
	}
	if p := New(Options{BaseURL: "https://example.test/"}); p.base != "https://example.test" {
		t.Fatalf("base %q", p.base)
	}
}

// ---------------------------------------------------------------------------
// Delivery reports
// ---------------------------------------------------------------------------

func TestParseStatusAssumedShape(t *testing.T) {
	p := newTestProvider("")
	p.now = func() time.Time { return time.Unix(1783425600, 0) }
	receipts, err := p.ParseStatus(dlrHTTP(testDLRToken, ""), fixture(t, "hubtel_sms_status_callback_assumed.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []channel.DeliveryReceipt{{Provider: Name, ProviderMessageID: docMsgID, Status: "delivered", At: time.Unix(1783425600, 0)}}
	if !reflect.DeepEqual(receipts, want) {
		t.Fatalf("got %+v\nwant %+v", receipts, want)
	}
}

func TestParseStatusShapes(t *testing.T) {
	p := newTestProvider("")
	post := func(body string) *http.Request { return dlrHTTP(testDLRToken, body) }
	get := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/webhooks/sms/hubtel/status?"+TokenParam+"="+testDLRToken+"&"+query, nil)
	}
	cases := []struct {
		name string
		r    *http.Request
		body string
		want []string // "id:status" per receipt
	}{
		{"object", post(""), `{"MessageId":"a","Status":"Delivered"}`, []string{"a:delivered"}},
		{"lower case keys", post(""), `{"messageId":"a","status":"delivered"}`, []string{"a:delivered"}},
		{"array", post(""), `[{"MessageId":"a","Status":"Delivered"},{"MessageId":"b","Status":"Failed"}]`, []string{"a:delivered", "b:failed"}},
		{"wrapped in Data", post(""), `{"ResponseCode":"0000","Data":{"MessageId":"a","Status":"Sent"}}`, []string{"a:submitted"}},
		{"wrapped list", post(""), `{"Data":[{"MessageId":"a","Status":"Undeliverable"}]}`, []string{"a:failed"}},
		{"extra fields", post(""), `{"MessageId":"a","Status":"Rejected","ClientReference":"msg-1","NetworkId":"62002","Rate":1,"Units":1}`, []string{"a:rejected"}},
		{"query string", get("MessageId=a&Status=Delivered&ClientReference=msg-1"), ``, []string{"a:delivered"}},
		{"form body", post(""), `MessageId=a&Status=Pending`, []string{"a:submitted"}},
		{"query string wins over empty body", get("messageid=a&status=expired"), `{}`, []string{"a:expired"}},
	}
	for _, c := range cases {
		receipts, err := p.ParseStatus(c.r, []byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var got []string
		for _, r := range receipts {
			if r.Provider != Name {
				t.Errorf("%s: provider %q", c.name, r.Provider)
			}
			got = append(got, r.ProviderMessageID+":"+r.Status)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseStatusErrors(t *testing.T) {
	p := newTestProvider("")
	for name, body := range map[string]string{
		"empty":              ``,
		"no status":          `{"MessageId":"a"}`,
		"no message id":      `{"Status":"Delivered","ClientReference":"msg-1"}`,
		"only client ref":    `{"ClientReference":"msg-1","Status":"Delivered"}`,
		"empty list":         `[]`,
		"not json or form":   `{"MessageId":`,
		"id too long":        `{"MessageId":"` + strings.Repeat("x", 200) + `","Status":"Delivered"}`,
		"scalars only":       `"Delivered"`,
		"status is an array": `{"MessageId":"a","Status":["Delivered"]}`,
	} {
		if _, err := p.ParseStatus(dlrHTTP(testDLRToken, body), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
}

func TestParseStatusRequiresToken(t *testing.T) {
	body := fixture(t, "hubtel_sms_status_callback_assumed.json")
	allow := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	withPeer := func(r *http.Request, addr string) *http.Request { r.RemoteAddr = addr; return r }
	cases := []struct {
		name string
		p    *Provider
		r    *http.Request
		ok   bool
	}{
		{"valid token", newTestProvider(""), dlrHTTP(testDLRToken, ""), true},
		{"no token", newTestProvider(""), dlrHTTP("", ""), false},
		{"wrong token", newTestProvider(""), dlrHTTP("nope-0123456789abcdef0123456789", ""), false},
		{"USSD token does not open delivery reports", newTestProvider(""), dlrHTTP(testUSSDToken, ""), false},
		{"no DLR token configured", New(Options{USSDTokens: []string{testUSSDToken}}), dlrHTTP(testUSSDToken, ""), false},
		{"nothing configured", New(Options{}), dlrHTTP("", ""), false},
		{"token in body only", newTestProvider(""), dlrHTTP("", `{"token":"`+testDLRToken+`"}`), false},
		{"allowlisted peer", New(Options{DLRTokens: []string{testDLRToken}, DLRAllowedIPs: allow}), withPeer(dlrHTTP(testDLRToken, ""), "203.0.113.7:1"), true},
		{"peer outside allowlist", New(Options{DLRTokens: []string{testDLRToken}, DLRAllowedIPs: allow}), withPeer(dlrHTTP(testDLRToken, ""), "198.51.100.7:1"), false},
		{"USSD allowlist does not apply", New(Options{DLRTokens: []string{testDLRToken}, USSDAllowedIPs: allow}), withPeer(dlrHTTP(testDLRToken, ""), "198.51.100.7:1"), true},
	}
	for _, c := range cases {
		_, err := c.p.ParseStatus(c.r, body)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", c.name, err)
		}
	}
}

// TestStatusMapping covers the statuses in Hubtel's delivery-status table.
func TestStatusMapping(t *testing.T) {
	cases := map[string][2]string{
		"Delivered":     {"delivered", ""},
		"DELIVERED":     {"delivered", ""},
		"Sent":          {"submitted", ""},
		"Pending":       {"submitted", ""},
		"Acknowledged":  {"submitted", ""},
		"Reprocessing":  {"submitted", ""},
		"Undeliverable": {"failed", "UNDELIVERABLE"},
		"Failed":        {"failed", "FAILED"},
		"Rejected":      {"rejected", "REJECTED"},
		"Not Delivered": {"failed", "NOT DELIVERED"},
		"Expired":       {"expired", "EXPIRED"},
		"0":             {"unknown", "0"},
		"Something New": {"unknown", "SOMETHING NEW"},
	}
	for raw, want := range cases {
		if st, code := mapStatus(raw); st != want[0] || code != want[1] {
			t.Errorf("%q: got (%q, %q), want %v", raw, st, code, want)
		}
	}
}

func TestParseInboundIsRefused(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/sms/hubtel/inbound", nil)
	if _, err := newTestProvider("").ParseInbound(r, []byte(`{}`)); !errors.Is(err, channel.ErrBadPayload) {
		t.Fatalf("got %v", err)
	}
}
