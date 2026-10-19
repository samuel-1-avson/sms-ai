package mtn

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"smsaighana/internal/channel"
	"smsaighana/internal/msisdn"
)

func TestNotAStatusQuerier(t *testing.T) {
	// The V2 lookup needs an id that a lost send
	// response takes with it; "not found" would make the reconciler resend.
	if _, ok := any(New(Options{})).(channel.StatusQuerier); ok {
		t.Fatal("Provider must not implement channel.StatusQuerier (see the package comment)")
	}
}

func TestParseSessionBegin(t *testing.T) {
	p := newTestProvider("")
	// Built from the per-field examples of MTN's InboundRequest schema.
	ex, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "mtn_ussd_begin_swagger_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantEx := channel.USSDRequest{Provider: Name, SessionID: "01235", FromE164: "2252312345", NewSession: true, Network: "mtn"}
	if ex != wantEx {
		t.Fatalf("swagger example:\n got %+v\nwant %+v", ex, wantEx)
	}

	req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "mtn_ussd_begin_constructed.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := channel.USSDRequest{Provider: Name, SessionID: "9a3c0e51b5b64f0f", FromE164: "233541234567", NewSession: true, Network: "mtn"}
	if req != want {
		t.Fatalf("begin:\n got %+v\nwant %+v", req, want)
	}
	// The engine normalizes the number MTN sends without a "+".
	if e164, err := msisdn.Normalize(req.FromE164); err != nil || e164 != "+233541234567" {
		t.Fatalf("engine cannot normalize msisdn: %q %v", e164, err)
	}
	// The dialled code is not a menu entry and does not key the conversation.
	if req.Input != "" || req.ServiceCode != "" {
		t.Fatalf("begin must carry no input or service code: %+v", req)
	}
}

func TestParseSessionContinue(t *testing.T) {
	p := newTestProvider("")
	req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "mtn_ussd_continue_constructed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if req.NewSession || req.Input != "What is photosynthesis?" || req.SessionID != "9a3c0e51b5b64f0f" || req.FromE164 != "233541234567" {
		t.Fatalf("continue: %+v", req)
	}

	// By default the whole string is the latest entry, so a "*" inside a
	// question stays intact.
	body := `{"sessionId":"s1","messageType":"1","msisdn":"233541234567","serviceCode":"*713*1#","ussdString":"Is 2*3 equal to 6?"}`
	if got, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); err != nil || got.Input != "Is 2*3 equal to 6?" {
		t.Fatalf("latest entry: %+v %v", got, err)
	}

	// With MTN_USSD_INPUT_IS_HISTORY only the part after the last "*" is used.
	hist := New(Options{USSDTokens: []string{testUSSDToken}, USSDInputIsHistory: true})
	body = `{"sessionId":"s1","messageType":"1","msisdn":"233541234567","ussdString":"1*2*What is a cedi?"}`
	if got, err := hist.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); err != nil || got.Input != "What is a cedi?" {
		t.Fatalf("history mode: %+v %v", got, err)
	}
	body = `{"sessionId":"s1","messageType":"1","msisdn":"233541234567","ussdString":"2"}`
	if got, err := hist.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); err != nil || got.Input != "2" {
		t.Fatalf("history mode, single entry: %+v %v", got, err)
	}
}

func TestEndCancelAndTimeoutEndTheSession(t *testing.T) {
	// They arrive as the engine's Exit entry, never as a new session or a question.
	p := newTestProvider("")
	for _, name := range []string{"mtn_ussd_end_constructed.json", "mtn_ussd_timeout_constructed.json"} {
		req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if req.NewSession || req.Input != "0" || req.SessionID != "9a3c0e51b5b64f0f" {
			t.Errorf("%s: %+v", name, req)
		}
	}
	for _, mt := range []string{`"2"`, `"4"`, `"5"`, `2`, `4`, `5`} {
		body := `{"sessionId":"s1","messageType":` + mt + `,"msisdn":"233541234567","ussdString":"Aborted by the user"}`
		req, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(body))
		if err != nil || req.Input != "0" || req.NewSession {
			t.Errorf("messageType %s: %+v %v", mt, req, err)
		}
	}
}

func TestParseSessionRejectsBadPayloads(t *testing.T) {
	p := newTestProvider("")
	cases := map[string]string{
		"not JSON":              `garbage`,
		"empty object":          `{}`,
		"no session id":         `{"messageType":"0","msisdn":"233541234567"}`,
		"no msisdn":             `{"sessionId":"s1","messageType":"0"}`,
		"no message type":       `{"sessionId":"s1","msisdn":"233541234567"}`,
		"notification":          `{"sessionId":"s1","messageType":"3","msisdn":"233541234567","ussdString":"Promo"}`,
		"unknown message type":  `{"sessionId":"s1","messageType":"9","msisdn":"233541234567"}`,
		"textual message type":  `{"sessionId":"s1","messageType":"Begin","msisdn":"233541234567"}`,
		"message type is a map": `{"sessionId":"s1","messageType":{"a":1},"msisdn":"233541234567"}`,
		"array":                 `[{"sessionId":"s1"}]`,
	}
	for name, body := range cases {
		if _, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
}

func TestUSSDAuthentication(t *testing.T) {
	body := fixture(t, "mtn_ussd_begin_constructed.json")
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
		{"delivery-notification token does not open USSD", newTestProvider(""), ussdHTTP(testDLRToken), false},
		{"token in path is not enough", newTestProvider(""), httptest.NewRequest(http.MethodPost, USSDCallbackPath+"/"+testUSSDToken, nil), false},
		{"token in a header is not enough", newTestProvider(""), func() *http.Request {
			r := ussdHTTP("")
			r.Header.Set("Authorization", "Bearer "+testUSSDToken)
			r.Header.Set("x-api-key", testUSSDToken)
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
	req, err := p.ParseSession(ussdHTTP(testUSSDToken), fixture(t, "mtn_ussd_begin_constructed.json"))
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

	// The continue reply is the constructed InboundResponse fixture.
	rec, con := write(channel.USSDReply{Message: "SMS AI Ghana\nType your question:\n1.Help 2.New chat"})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if want := fixtureMap(t, "mtn_ussd_response_constructed.json"); !reflect.DeepEqual(con, want) {
		t.Fatalf("continue reply:\n got %v\nwant %v", con, want)
	}

	rec, end := write(channel.USSDReply{Message: "Thanks! <Bye> & see you", End: true})
	data, _ := end["data"].(map[string]any)
	if end["statusCode"] != "0000" || data["messageType"] != float64(2) || data["userInputRequired"] != false ||
		data["inboundResponse"] != "Thanks! <Bye> & see you" || data["msisdn"] != "233541234567" {
		t.Fatalf("end reply: %v", end)
	}
	if !strings.Contains(rec.Body.String(), "Thanks! <Bye> & see you") {
		t.Fatalf("message was HTML-escaped: %s", rec.Body.String())
	}
	// Every field the schema marks required is present on both replies.
	for _, m := range []map[string]any{con, end} {
		for _, k := range []string{"statusCode", "statusMessage", "transactionId", "data", "_link"} {
			if _, ok := m[k]; !ok {
				t.Errorf("required field %q missing in %v", k, m)
			}
		}
		link, _ := m["_link"].(map[string]any)
		self, _ := link["self"].(map[string]any)
		if self["href"] == "" || self["href"] == nil {
			t.Errorf("_link.self.href missing in %v", m)
		}
	}
	// The callback token must never be echoed back.
	if strings.Contains(rec.Body.String(), testUSSDToken) {
		t.Fatal("reply contains the callback token")
	}
	// Without a configured service code the field is left out, not empty.
	bare := New(Options{})
	rec = httptest.NewRecorder()
	if err := bare.WriteReply(rec, req, channel.USSDReply{Message: "hi"}); err != nil || strings.Contains(rec.Body.String(), "serviceCode") {
		t.Fatalf("reply without service code: %s %v", rec.Body.String(), err)
	}
}

func TestRedactURL(t *testing.T) {
	got := RedactURL("https://sms.example.com/webhooks/ussd/mtn/session?token=" + testUSSDToken + "&x=1")
	if strings.Contains(got, testUSSDToken) || !strings.Contains(got, "token=REDACTED") || !strings.Contains(got, "x=1") {
		t.Fatalf("redacted URL %q", got)
	}
	if got := RedactURL("https://sms.example.com/inbound"); got != "https://sms.example.com/inbound" {
		t.Fatalf("URL without token changed: %q", got)
	}
}

func TestCallbackURL(t *testing.T) {
	u, err := CallbackURL("https://sms.example.com/", USSDCallbackPath, "a+b/c d")
	if err != nil || u != "https://sms.example.com/webhooks/ussd/mtn/session?token=a%2Bb%2Fc+d" {
		t.Fatalf("got %q %v", u, err)
	}
	if u, err := CallbackURL("https://sms.example.com", DLRCallbackPath, ""); err != nil || strings.Contains(u, "?") {
		t.Fatalf("no token: %q %v", u, err)
	}
	for _, bad := range []string{"", "sms.example.com", "/relative", "ftp://x"} {
		if _, err := CallbackURL(bad, USSDCallbackPath, "t"); err == nil {
			t.Errorf("CallbackURL(%q) should fail", bad)
		}
	}
}
