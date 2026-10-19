package mnotify

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/channel"
)

func parse(t *testing.T, p *Provider, body []byte) channel.USSDRequest {
	t.Helper()
	req, err := p.ParseSession(ussdHTTP(testUSSDToken), body)
	if err != nil {
		t.Fatalf("ParseSession(%s): %v", body, err)
	}
	return req
}

func TestSharedRequestFromTheOpenAPIExample(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	req := parse(t, p, fixture(t, "mnotify_ussd_shared_request_openapi_example.json"))
	want := channel.USSDRequest{
		Provider: Name, SessionID: "233541509394", FromE164: "233541509394", NewSession: true, Network: "shared|6789940010",
	}
	if req != want {
		t.Fatalf("got %+v\nwant %+v", req, want)
	}
}

func TestSharedContinuation(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	req := parse(t, p, fixture(t, "mnotify_ussd_shared_continue_constructed.json"))
	// The session is keyed by the phone number, since there is no session id,
	// and the input is the user's entry.
	if req.NewSession || req.Input != "What is photosynthesis?" || req.SessionID != "233541509394" || req.Network != "shared|6789940011" {
		t.Fatalf("got %+v", req)
	}
	// The same number written differently is the same session.
	m := fixtureMap(t, "mnotify_ussd_shared_continue_constructed.json")
	m["msisdn"] = "+233 54 150 9394"
	b, _ := json.Marshal(m)
	if got := parse(t, p, b); got.SessionID != req.SessionID || got.FromE164 != "+233 54 150 9394" {
		t.Fatalf("got %+v", got)
	}
}

func TestSharedNewSessionDetection(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	for data, wantNew := range map[string]bool{
		"*899*86#":     true,
		"*899#":        true,
		" *899*86# ":   true,
		"*899*86*1#":   true,
		"1":            false,
		"2":            false,
		"0":            false,
		"":             false,
		"hello there":  false,
		"* what is it": false, // a question that starts with "*" has spaces; a dial string never does
		"What is *?":   false,
	} {
		req := parse(t, p, []byte(`{"msisdn":"233541509394","sequenceID":"1","data":`+jsonString(data)+`,"timestamp":"t"}`))
		if req.NewSession != wantNew {
			t.Errorf("data %q: NewSession=%v, want %v", data, req.NewSession, wantNew)
		}
		if wantNew && req.Input != "" {
			t.Errorf("data %q: a dialled string is not a menu entry, got input %q", data, req.Input)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestSharedSessionEndsWhenWeEndIt: after a reply with continueFlag 1 the next
// request from that number starts a new session, whatever it holds.
func TestSharedSessionEndsWhenWeEndIt(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	step := func(msisdn, data string) channel.USSDRequest {
		return parse(t, p, []byte(`{"msisdn":"`+msisdn+`","sequenceID":"1","data":`+jsonString(data)+`,"timestamp":"t"}`))
	}
	reply := func(req channel.USSDRequest, end bool) {
		t.Helper()
		if err := p.WriteReply(httptest.NewRecorder(), req, channel.USSDReply{Message: "x", End: end}); err != nil {
			t.Fatal(err)
		}
	}

	first := step("233541509394", "*899*86#")
	reply(first, false)
	cont := step("233541509394", "hello")
	if cont.NewSession {
		t.Fatal("a continuation was read as a new session")
	}
	reply(cont, true) // the engine ended the session

	// A request after the END, even without a dial string, starts a new session...
	after := step("233541509394", "1")
	if !after.NewSession || after.Input != "" {
		t.Fatalf("a request after the session ended must start a new one: %+v", after)
	}
	reply(after, false)
	// ...once: the new session then continues normally.
	if again := step("233541509394", "1"); again.NewSession {
		t.Fatalf("the memory of the ended session was not cleared: %+v", again)
	}

	// Other numbers are unaffected.
	reply(step("233201234567", "*899*86#"), true)
	if other := step("233541509394", "1"); other.NewSession {
		t.Fatalf("one number's END affected another: %+v", other)
	}

	// The memory is bounded in time: an END from long ago is forgotten.
	reply(step("233541509394", "bye"), true)
	now = now.Add(endedTTL + time.Second)
	if late := step("233541509394", "2"); late.NewSession {
		t.Fatalf("an old END was remembered: %+v", late)
	}

	// And bounded in size.
	e := newEndedSessions()
	for i := 0; i < endedMax+5; i++ {
		e.mark(strconv.Itoa(i), now)
	}
	e.mu.Lock()
	n := len(e.m)
	e.mu.Unlock()
	if n > endedMax {
		t.Fatalf("ended-session memory grew to %d", n)
	}
}

func TestDedicatedRequestFromTheOpenAPIExample(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	req := parse(t, p, fixture(t, "mnotify_ussd_dedicated_request_openapi_example.json"))
	want := channel.USSDRequest{
		Provider: Name, SessionID: "ABC123456", FromE164: "233541509394", Input: "1", Network: "dedicated|mtn",
	}
	if req != want {
		t.Fatalf("got %+v\nwant %+v", req, want)
	}
	begin := parse(t, p, fixture(t, "mnotify_ussd_dedicated_begin_constructed.json"))
	if !begin.NewSession || begin.Input != "" || begin.SessionID != "SESS-0001" {
		t.Fatalf("got %+v", begin)
	}
	// The three networks named by the file.
	for _, gw := range []string{"MTN", "Vodafone", "AirtelTigo"} {
		m := fixtureMap(t, "mnotify_ussd_dedicated_request_openapi_example.json")
		m["ussdGwId"] = gw
		b, _ := json.Marshal(m)
		if got := parse(t, p, b); got.Network != "dedicated|"+strings.ToLower(gw) {
			t.Errorf("network %q", got.Network)
		}
	}
}

func TestPayloadShapeAndModes(t *testing.T) {
	shared := fixture(t, "mnotify_ussd_shared_request_openapi_example.json")
	dedicated := fixture(t, "mnotify_ussd_dedicated_request_openapi_example.json")
	for _, c := range []struct {
		mode      string
		body      []byte
		wantError bool
	}{
		{"", shared, false}, {"", dedicated, false},
		{ModeAuto, shared, false}, {ModeAuto, dedicated, false},
		{ModeShared, shared, false}, {ModeShared, dedicated, true},
		{ModeDedicated, dedicated, false}, {ModeDedicated, shared, true},
	} {
		p := New(Options{USSDMode: c.mode, USSDTokens: []string{testUSSDToken}, Log: quietLog()})
		_, err := p.ParseSession(ussdHTTP(testUSSDToken), c.body)
		if (err != nil) != c.wantError || err != nil && !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("mode %q: error %v, want error=%v", c.mode, err, c.wantError)
		}
	}

	p := newTestProvider("http://127.0.0.1:1/api")
	for name, body := range map[string]string{
		"not json":             `nope`,
		"json null":            `null`,
		"json array":           `[]`,
		"empty object":         `{}`,
		"shared without data":  `{"msisdn":"233541509394","sequenceID":"1"}`,
		"shared without seq":   `{"msisdn":"233541509394","data":"1"}`,
		"shared without phone": `{"sequenceID":"1","data":"1"}`,
		"dedicated no session": `{"shortCode":"*1#","msIsdn":"233541509394","text":"1","ussdGwId":"MTN"}`,
		"dedicated no text":    `{"shortCode":"*1#","msIsdn":"233541509394","sessId":"S","ussdGwId":"MTN"}`,
		"dedicated no phone":   `{"shortCode":"*1#","text":"1","sessId":"S"}`,
		"both shapes":          `{"msisdn":"233541509394","sequenceID":"1","data":"1","sessId":"S","text":"1"}`,
		"only a _note":         `{"_note":"constructed"}`,
	} {
		if _, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
}

// TestPayloadKeysAreCaseInsensitive: the file spells the number "msisdn" in one
// product and "msIsdn" in the other, and Go's decoder would conflate them, so
// the adapter matches keys itself and tolerates other casings.
func TestPayloadKeysAreCaseInsensitive(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	req := parse(t, p, []byte(`{"MSISDN":"233541509394","SequenceId":"7","DATA":"2","Timestamp":"t"}`))
	if req.SessionID != "233541509394" || req.Input != "2" || req.Network != "shared|7" {
		t.Fatalf("got %+v", req)
	}
	req = parse(t, p, []byte(`{"MSISDN":"233541509394","SESSID":"S9","Text":"3","ShortCode":"*1#"}`))
	if req.SessionID != "S9" || req.Input != "3" || !strings.HasPrefix(req.Network, "dedicated|") {
		t.Fatalf("got %+v", req)
	}
	// Numbers where strings are documented.
	req = parse(t, p, []byte(`{"msisdn":233541509394,"sequenceID":6789940010,"data":1}`))
	if req.FromE164 != "233541509394" || req.Network != "shared|6789940010" || req.Input != "1" {
		t.Fatalf("got %+v", req)
	}
	// Unknown extra fields (the dedicated mode has "optional" and "imsi") are ignored.
	req = parse(t, p, []byte(`{"msIsdn":"233541509394","sessId":"S","text":"1","imsi":"620011234567890","optional":"x","language":"en"}`))
	if req.SessionID != "S" {
		t.Fatalf("got %+v", req)
	}
}

func TestCallbackAuthentication(t *testing.T) {
	body := fixture(t, "mnotify_ussd_shared_request_openapi_example.json")
	other := "other-token-0123456789abcdef0123456789"
	p := New(Options{USSDTokens: []string{testUSSDToken, other}, Log: quietLog()})
	for name, c := range map[string]struct {
		url string
		ok  bool
	}{
		"first token":         {USSDCallbackPath + "?token=" + testUSSDToken, true},
		"second token":        {USSDCallbackPath + "?token=" + other, true},
		"no token":            {USSDCallbackPath, false},
		"empty token":         {USSDCallbackPath + "?token=", false},
		"wrong token":         {USSDCallbackPath + "?token=" + testUSSDToken + "x", false},
		"prefix of a token":   {USSDCallbackPath + "?token=" + testUSSDToken[:10], false},
		"token in other name": {USSDCallbackPath + "?key=" + testUSSDToken, false},
		"escaped token":       {USSDCallbackPath + "?token=" + strings.ReplaceAll(testUSSDToken, "-", "%2D"), true},
	} {
		_, err := p.ParseSession(httptest.NewRequest(http.MethodPost, c.url, nil), body)
		if c.ok && err != nil || !c.ok && !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The API key is not a callback credential.
	pk := New(Options{APIKey: testKey, USSDTokens: []string{testUSSDToken}, Log: quietLog()})
	if _, err := pk.ParseSession(httptest.NewRequest(http.MethodPost, USSDCallbackPath+"?token="+testKey, nil), body); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("the API key was accepted as a callback token: %v", err)
	}
	// A bad token wins over a bad payload: nothing is learned without it.
	if _, err := p.ParseSession(ussdHTTP("wrong"), []byte(`nope`)); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("unauthenticated garbage: %v", err)
	}
}

func TestNoTokenConfiguredRefusesEverything(t *testing.T) {
	body := fixture(t, "mnotify_ussd_shared_request_openapi_example.json")
	p := New(Options{Log: quietLog()})
	for _, token := range []string{"", "anything", testUSSDToken} {
		if _, err := p.ParseSession(ussdHTTP(token), body); !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("token %q with none configured: %v", token, err)
		}
	}
	// A blank entry in the list must not match an empty token.
	p = New(Options{USSDTokens: []string{""}, Log: quietLog()})
	if _, err := p.ParseSession(ussdHTTP(""), body); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("blank token entry: %v", err)
	}
}

func TestSourceAddressAllowlist(t *testing.T) {
	body := fixture(t, "mnotify_ussd_shared_request_openapi_example.json")
	allow := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}
	direct := New(Options{USSDTokens: []string{testUSSDToken}, USSDAllowedIPs: allow, Log: quietLog()})
	proxied := New(Options{USSDTokens: []string{testUSSDToken}, USSDAllowedIPs: allow, ClientIPHeader: "X-Real-IP", Log: quietLog()})

	at := func(p *Provider, remote, header string) error {
		r := ussdHTTP(testUSSDToken)
		r.RemoteAddr = remote
		if header != "" {
			r.Header.Set("X-Real-IP", header)
		}
		_, err := p.ParseSession(r, body)
		return err
	}
	for name, err := range map[string]error{
		"allowed peer":          at(direct, "203.0.113.9:4444", ""),
		"allowed v6 peer":       at(direct, "[2001:db8::7]:4444", ""),
		"allowed mapped v4":     at(direct, "[::ffff:203.0.113.9]:4444", ""),
		"allowed header":        at(proxied, "10.0.0.2:1", "203.0.113.200"),
		"header, last hop used": at(proxied, "10.0.0.2:1", "198.51.100.1, 203.0.113.200"),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"outside peer":            at(direct, "198.51.100.1:4444", ""),
		"unparseable peer":        at(direct, "not-an-address", ""),
		"outside header":          at(proxied, "203.0.113.9:1", "198.51.100.1"),
		"spoofed first hop":       at(proxied, "203.0.113.9:1", "203.0.113.200, 198.51.100.1"),
		"missing header":          at(proxied, "203.0.113.9:1", ""),
		"header ignored when off": at(direct, "198.51.100.1:1", "203.0.113.5"),
	} {
		if !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", name, err)
		}
	}
	// The allowlist never replaces the token.
	r := callbackHTTP(http.MethodPost, USSDCallbackPath, "", nil)
	r.RemoteAddr = "203.0.113.9:1"
	if _, err := direct.ParseSession(r, body); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("allowlisted address without a token: %v", err)
	}
}

func write(t *testing.T, p *Provider, req channel.USSDRequest, reply channel.USSDReply) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := p.WriteReply(rec, req, reply); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, rec.Body.String())
	}
	return rec, out
}

func TestSharedReply(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	now := time.Date(2025, 9, 1, 11, 22, 34, 0, time.UTC)
	p.now = func() time.Time { return now }
	req := parse(t, p, fixture(t, "mnotify_ussd_shared_request_openapi_example.json"))

	// The engine writes \n between lines; mNotify wants \r\n.
	rec, out := write(t, p, req, channel.USSDReply{Message: "SMS AI Ghana\nType your question:\n1.Help 2.New chat"})
	wantMsg := "SMS AI Ghana\r\nType your question:\r\n1.Help 2.New chat"
	if out["message"] != wantMsg {
		t.Fatalf("message %q, want %q", out["message"], wantMsg)
	}
	if out["msisdn"] != "233541509394" || out["sequenceID"] != "6789940010" || out["continueFlag"] != float64(0) ||
		out["timestamp"] != "2025-09-01T11:22:34Z" {
		t.Fatalf("reply %v", out)
	}
	// On the wire the line breaks are JSON escapes, and no bare newline is left
	// inside the message.
	if !strings.Contains(rec.Body.String(), `SMS AI Ghana\r\nType your question:\r\n1.Help`) {
		t.Fatalf("wire format: %s", rec.Body.String())
	}
	if strings.Contains(strings.TrimSpace(rec.Body.String()), "\n") {
		t.Fatalf("raw newline inside the JSON: %q", rec.Body.String())
	}
	// Only the documented fields.
	for k := range out {
		switch k {
		case "msisdn", "sequenceID", "message", "timestamp", "continueFlag":
		default:
			t.Errorf("undocumented reply field %q", k)
		}
	}

	_, out = write(t, p, req, channel.USSDReply{Message: "Goodbye", End: true})
	if out["continueFlag"] != float64(1) || out["message"] != "Goodbye" {
		t.Fatalf("end reply %v", out)
	}

	// The OpenAPI example reply has the same fields and echoes the same phone
	// number. (Its sequenceID and timestamp values are swapped in the file, so
	// those two are not compared.)
	var example map[string]any
	if err := json.Unmarshal(fixture(t, "mnotify_ussd_shared_response_openapi_example.json"), &example); err != nil {
		t.Fatal(err)
	}
	delete(example, "_note")
	_, out = write(t, p, req, channel.USSDReply{Message: "WHAT YOU WANT USERS TO READ."})
	if len(out) != len(example) || out["msisdn"] != example["msisdn"] || out["message"] != example["message"] || out["continueFlag"] != example["continueFlag"] {
		t.Fatalf("reply %v differs from the OpenAPI example %v", out, example)
	}
	for k := range example {
		if _, ok := out[k]; !ok {
			t.Errorf("reply lacks the example's field %q", k)
		}
	}

	// Existing carriage returns are not doubled; lone ones become \r\n too.
	_, out = write(t, p, req, channel.USSDReply{Message: "a\r\nb\rc\nd"})
	if out["message"] != "a\r\nb\r\nc\r\nd" {
		t.Fatalf("message %q", out["message"])
	}
	// Text is not HTML-escaped (the engine uses & and <).
	rec, out = write(t, p, req, channel.USSDReply{Message: "R&D <3"})
	if out["message"] != "R&D <3" || strings.Contains(rec.Body.String(), "u0026") {
		t.Fatalf("escaping: %s", rec.Body.String())
	}
}

func TestDedicatedReply(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	req := parse(t, p, fixture(t, "mnotify_ussd_dedicated_request_openapi_example.json"))

	// The OpenAPI example response, reproduced from \n-separated engine text.
	var example map[string]any
	if err := json.Unmarshal(fixture(t, "mnotify_ussd_dedicated_response_openapi_example.json"), &example); err != nil {
		t.Fatal(err)
	}
	rec, out := write(t, p, req, channel.USSDReply{Message: "1. Balance\n2. Buy Data\n0. Exit"})
	if out["ussdMenu"] != example["ussdMenu"] || out["shouldClose"] != false || out["responseExitCode"] != float64(200) {
		t.Fatalf("got %v, example %v", out, example)
	}
	if !strings.Contains(rec.Body.String(), `1. Balance\r\n2. Buy Data\r\n0. Exit`) {
		t.Fatalf("wire format: %s", rec.Body.String())
	}
	if msg, _ := out["responseMessage"].(string); msg == "" {
		t.Fatalf("responseMessage missing: %v", out)
	}
	for k := range out {
		switch k {
		case "responseExitCode", "shouldClose", "ussdMenu", "responseMessage":
		default:
			t.Errorf("undocumented reply field %q", k)
		}
	}
	_, out = write(t, p, req, channel.USSDReply{Message: "Goodbye", End: true})
	if out["shouldClose"] != true || out["ussdMenu"] != "Goodbye" {
		t.Fatalf("end reply %v", out)
	}
}

func TestReplyWithoutContextIsAnError(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	for _, network := range []string{"", "mnotify", "unknown|x"} {
		rec := httptest.NewRecorder()
		err := p.WriteReply(rec, channel.USSDRequest{Provider: Name, FromE164: "233541509394", Network: network}, channel.USSDReply{Message: "x"})
		if err == nil || rec.Body.Len() != 0 {
			t.Errorf("network %q: want an error and no body, got %v / %q", network, err, rec.Body.String())
		}
	}
}

func TestCRLF(t *testing.T) {
	for in, want := range map[string]string{
		"":              "",
		"one line":      "one line",
		"a\nb":          "a\r\nb",
		"a\r\nb":        "a\r\nb",
		"a\rb":          "a\r\nb",
		"a\n\nb":        "a\r\n\r\nb",
		"a\r\n\r\nb\n":  "a\r\n\r\nb\r\n",
		"\n1.Follow-up": "\r\n1.Follow-up",
	} {
		if got := crlf(in); got != want {
			t.Errorf("crlf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionKey(t *testing.T) {
	for in, want := range map[string]string{
		"233541509394": "233541509394", "+233541509394": "233541509394", "+233 54-150 9394": "233541509394", "": "",
	} {
		if got := sessionKey(in); got != want {
			t.Errorf("sessionKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUSSDIsRaceFree hammers the shared state (the ended-session memory)
// from many goroutines; it earns its keep under -race.
func TestUSSDIsRaceFree(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1/api")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				msisdn := "23354150" + string(rune('0'+g)) + "00" + string(rune('0'+i%10))
				req, err := p.ParseSession(ussdHTTP(testUSSDToken), []byte(`{"msisdn":"`+msisdn+`","sequenceID":"1","data":"1","timestamp":"t"}`))
				if err != nil {
					t.Error(err)
					return
				}
				if err := p.WriteReply(httptest.NewRecorder(), req, channel.USSDReply{Message: "x", End: i%3 == 0}); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
