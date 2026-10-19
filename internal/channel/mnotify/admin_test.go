package mnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func runAdmin(t *testing.T, cfgBase string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunAdmin(context.Background(), args[0], args[1:], adminConfig(t, cfgBase), &out)
	if leaksKey(out.String()) || err != nil && leaksKey(errChain(err)) {
		t.Fatalf("the API key leaked: %q / %v", out.String(), err)
	}
	return out.String(), err
}

func TestIsAdminCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"mnotify-status": true, "mnotify-sender-status": true, "mtn-status": false, "migrate": false, "": false, "mnotify": false,
	} {
		if IsAdminCommand(cmd) != want {
			t.Errorf("IsAdminCommand(%q) != %v", cmd, want)
		}
	}
	if err := RunAdmin(context.Background(), "mnotify-nope", nil, adminConfig(t, "http://127.0.0.1:1/api"), &bytes.Buffer{}); err == nil {
		t.Error("unknown command accepted")
	}
}

// TestStatusMakesNoCallWithoutTheFlag: the plain status command prints the
// configuration and touches nothing.
func TestStatusMakesNoCallWithoutTheFlag(t *testing.T) {
	fake := newFakeMNotify(t, respond(500, "{}"))
	out, err := runAdmin(t, fake.base(), "mnotify-status")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.recorded()); n != 0 {
		t.Fatalf("%d API calls without -check-credentials", n)
	}
	for _, want := range []string{
		"PRIMARY_SMS_PROVIDER=mnotify", "SMS_ENVIRONMENT=live", "API key            set", "SMS sender ID      SMS-AI",
		"USSD mode          auto", USSDCallbackPath + "?token=<token>", "1 token(s) set", "no delivery-report webhook",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	// Secrets are never printed, not even the callback token.
	for _, secret := range []string{testKey, testUSSDToken} {
		if strings.Contains(out, secret) {
			t.Fatalf("status printed a secret:\n%s", out)
		}
	}
}

func TestStatusShowsMissingSecrets(t *testing.T) {
	cfg := adminConfig(t, "http://127.0.0.1:1/api", "MNOTIFY_API_KEY=REPLACE_ME", "MNOTIFY_USSD_TOKEN=", "MNOTIFY_CLIENT_IP_HEADER=X-Real-IP", "MNOTIFY_USSD_ALLOWED_IPS=203.0.113.0/24")
	var out bytes.Buffer
	if err := RunAdmin(context.Background(), "mnotify-status", nil, cfg, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"API key            NOT SET", "0 token(s) NOT SET", "1 allowlist", "X-Real-IP", "not an mNotify host"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
	// With the flag but no key: an error, and no request.
	fake := newFakeMNotify(t, respond(200, "{}"))
	cfg = adminConfig(t, fake.base(), "MNOTIFY_API_KEY=")
	err := RunAdmin(context.Background(), "mnotify-status", []string{"-check-credentials"}, cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "MNOTIFY_API_KEY is not set") || len(fake.recorded()) != 0 {
		t.Fatalf("check without a key: %v (%d calls)", err, len(fake.recorded()))
	}
}

func TestStatusDoesNotPrintAKeyPastedIntoTheBaseURL(t *testing.T) {
	out, err := runAdmin(t, "https://api.mnotify.com/api?key="+testKey+"#frag", "mnotify-status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "https://api.mnotify.com/api (mNotify production") {
		t.Fatalf("status output:\n%s", out)
	}
	// Same through a URL with the key as credentials; runAdmin itself fails the
	// test if the key shows up in the output.
	_, _ = runAdmin(t, "https://"+testKey+"@api.mnotify.com/api", "mnotify-status")
}

func TestCheckCredentials(t *testing.T) {
	fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "mnotify_balance_sms_response_openapi_example.json"))
	})
	out, err := runAdmin(t, fake.base(), "mnotify-status", "-check-credentials")
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/api/balance/sms" ||
		len(calls[0].Query) != 1 || calls[0].Query.Get("key") != testKey || len(calls[0].Body) != 0 {
		t.Fatalf("calls: %+v", calls)
	}
	if !strings.Contains(out, "SMS balance 1000 credits, bonus 471") {
		t.Fatalf("balance not printed:\n%s", out)
	}
	if strings.Contains(out, "LOW") {
		t.Fatalf("a healthy balance was called low:\n%s", out)
	}

	low := newFakeMNotify(t, respond(200, `{"status":"success","balance":12}`))
	out, err = runAdmin(t, low.base(), "mnotify-status", "-check-credentials")
	if err != nil || !strings.Contains(out, "SMS balance 12 credits") || strings.Contains(out, "bonus") || !strings.Contains(out, "LOW") {
		t.Fatalf("low balance: %v\n%s", err, out)
	}
}

func TestCheckCredentialsFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"rejected key":              {401, `{"status":"error","message":"Invalid API key"}`},
		"error in a 200":            {200, `{"status":"error","message":"Invalid API key"}`},
		"success without a balance": {200, `{"status":"success"}`},
		"html":                      {200, `<html>`},
		"server error":              {500, `{"message":"oops"}`},
	} {
		fake := newFakeMNotify(t, respond(c.status, c.body))
		out, err := runAdmin(t, fake.base(), "mnotify-status", "-check-credentials")
		if err == nil || !strings.Contains(err.Error(), "credential check failed") {
			t.Errorf("%s: want a failure, got %v\n%s", name, err, out)
		}
		if strings.Contains(out, "accepted the key") {
			t.Errorf("%s: claimed success:\n%s", name, out)
		}
		if n := len(fake.recorded()); n != 1 {
			t.Errorf("%s: %d calls", name, n)
		}
	}
	// A refused connection.
	if _, err := runAdmin(t, "http://127.0.0.1:1/api", "mnotify-status", "-check-credentials"); err == nil {
		t.Error("connection refused was not reported")
	}
	// A bad flag.
	if _, err := runAdmin(t, "http://127.0.0.1:1/api", "mnotify-status", "-nope"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("bad flag: %v", err)
	}
}

func TestSenderStatus(t *testing.T) {
	fake := newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "mnotify_senderid_status_response_openapi_example.json"))
	})
	out, err := runAdmin(t, fake.base(), "mnotify-sender-status")
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/api/senderid/status" ||
		len(calls[0].Query) != 1 || calls[0].Query.Get("key") != testKey || calls[0].Header.Get("Content-Type") != "application/json" {
		t.Fatalf("calls: %+v", calls)
	}
	var body map[string]string
	if err := json.Unmarshal(calls[0].Body, &body); err != nil || len(body) != 1 || body["sender_name"] != "SMS-AI" {
		t.Fatalf("body %s: %v", calls[0].Body, err)
	}
	if !strings.Contains(out, `"SMS-AI" is Approved`) || strings.Contains(out, "not approved") {
		t.Fatalf("output:\n%s", out)
	}

	// -sender overrides the configured sender ID.
	fake2 := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"sender name":"mNotify","status":"Pending"}}`))
	var buf bytes.Buffer
	if err := RunAdmin(context.Background(), "mnotify-sender-status", []string{"-sender", "mNotify"}, adminConfig(t, fake2.base()), &buf); err != nil {
		t.Fatal(err)
	}
	var sent map[string]string
	_ = json.Unmarshal(fake2.recorded()[0].Body, &sent)
	if sent["sender_name"] != "mNotify" || !strings.Contains(buf.String(), `"mNotify" is Pending`) || !strings.Contains(buf.String(), "not approved") {
		t.Fatalf("sent %v, output:\n%s", sent, buf.String())
	}
}

func TestSenderStatusFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"rejected key":      {401, `{"status":"error","message":"Unauthenticated."}`},
		"unknown sender":    {200, `{"status":"error","code":"4000","message":"Sender ID not found"}`},
		"no status in body": {200, `{"status":"success","code":"2000","summary":{"sender name":"x"}}`},
		"summary is a list": {200, `{"status":"success","code":"2000","summary":[]}`},
		"redirect":          {301, ``},
	} {
		fake := newFakeMNotify(t, respond(c.status, c.body))
		out, err := runAdmin(t, fake.base(), "mnotify-sender-status")
		if err == nil || !strings.Contains(err.Error(), "sender ID lookup") {
			t.Errorf("%s: want a failure, got %v\n%s", name, err, out)
		}
		if n := len(fake.recorded()); n != 1 {
			t.Errorf("%s: %d calls", name, n)
		}
	}
	// Invalid input makes no call at all.
	fake := newFakeMNotify(t, respond(200, "{}"))
	for _, args := range [][]string{
		{"mnotify-sender-status", "-sender", ""},
		{"mnotify-sender-status", "-sender", "WAY-TOO-LONG-SENDER"},
		{"mnotify-sender-status", "-bogus"},
	} {
		if _, err := runAdmin(t, fake.base(), args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	var out bytes.Buffer
	if err := RunAdmin(context.Background(), "mnotify-sender-status", nil, adminConfig(t, fake.base(), "MNOTIFY_API_KEY="), &out); err == nil {
		t.Error("sender lookup without a key accepted")
	}
	if n := len(fake.recorded()); n != 0 {
		t.Errorf("%d calls for invalid input", n)
	}
}

// TestAdminCannotReachTheRealAPIFromTests: the test guard is the safety net
// behind "admin tests never call mNotify".
func TestAdminCannotReachTheRealAPIFromTests(t *testing.T) {
	for _, args := range [][]string{{"mnotify-status", "-check-credentials"}, {"mnotify-sender-status"}} {
		out, err := runAdmin(t, DefaultBaseURL, args...)
		if err == nil || !strings.Contains(err.Error(), "refusing to call an mNotify host") {
			t.Errorf("%v: the guard did not stop a call to the real host: %v\n%s", args, err, out)
		}
	}
}
