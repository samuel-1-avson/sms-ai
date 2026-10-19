package mtn

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"smsaighana/internal/config"
)

// recorded is one request the fake MTN received.
type recorded struct {
	Method, Path string
	Header       http.Header
	Body         map[string]any
}

// subscriptionFake serves both subscription APIs and records what it gets.
func subscriptionFake(t *testing.T) (*fakeMTN, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
		_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/ussd/subscription":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(fixture(t, "mtn_ussd_subscription_response_swagger_example.json"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/sms/outbound/SMS-AI/subscription":
			_, _ = w.Write(fixture(t, "mtn_sms_subscription_response_constructed.json"))
		case r.Method == http.MethodDelete && (r.URL.Path == "/v1/messages/ussd/subscription/123-xyz-456" ||
			r.URL.Path == "/v1/messages/sms/outbound/SMS-AI/subscription/sub-6328f1c2"):
			_, _ = io.WriteString(w, `{"statusCode":"0000","statusMessage":"Successful","transactionId":"t","data":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(fixture(t, "mtn_error_response_swagger_example.json"))
		}
	})
	return fake, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), reqs...)
	}
}

func adminConfig(t *testing.T, base string, extra ...string) *config.Config {
	t.Helper()
	environ := append([]string{
		"APP_ENV=test", "MTN_BASE_URL=" + base,
		"MTN_CLIENT_ID=" + testClientID, "MTN_CLIENT_SECRET=" + testSecret,
		"MTN_USSD_TOKEN=" + testUSSDToken, "MTN_DLR_TOKEN=" + testDLRToken,
		"MTN_CALLBACK_BASE_URL=https://sms.example.com", "USSD_SERVICE_CODE=*713*1#", "SMS_OUTBOUND_SENDER_ID=SMS-AI",
	}, extra...)
	cfg, err := config.LoadFrom(environ)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runAdmin(t *testing.T, cfg *config.Config, cmd string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunAdmin(context.Background(), cmd, args, cfg, &out)
	return out.String(), err
}

func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range []string{testUSSDToken, testDLRToken, testSecret, "tok-1", testClientID} {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains a secret (%q):\n%s", what, secret, text)
		}
	}
}

func TestSubscribeRegistersBothCallbacks(t *testing.T) {
	fake, requests := subscriptionFake(t)
	out, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-subscribe")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %+v", reqs)
	}

	u := reqs[0]
	if u.Method != http.MethodPost || u.Path != "/v1/messages/ussd/subscription" {
		t.Fatalf("USSD request: %+v", u)
	}
	// USSD uses the API key, which MTN's FAQ says is the Consumer Key. No bearer token.
	if u.Header.Get("x-api-key") != testClientID || u.Header.Get("Authorization") != "" || u.Header.Get("transactionId") == "" {
		t.Fatalf("USSD headers: %v", u.Header)
	}
	wantUSSD := map[string]any{
		"serviceCode": "*713*1#", "targetSystem": "sms-ai-ghana",
		"callbackUrl": "https://sms.example.com/webhooks/ussd/mtn/session?token=" + testUSSDToken,
	}
	if !jsonEqual(u.Body, wantUSSD) {
		t.Fatalf("USSD body:\n got %v\nwant %v", u.Body, wantUSSD)
	}

	s := reqs[1]
	if s.Method != http.MethodPost || s.Path != "/v1/messages/sms/outbound/SMS-AI/subscription" {
		t.Fatalf("SMS request: %+v", s)
	}
	if s.Header.Get("Authorization") != "Bearer tok-1" || s.Header.Get("x-api-key") != "" ||
		s.Header.Get("transactionId") == "" || s.Header.Get("x-country-code") != "GH" {
		t.Fatalf("SMS headers: %v", s.Header)
	}
	wantSMS := map[string]any{
		"targetSystem": "sms-ai-ghana",
		"notifyUrl":    "https://sms.example.com/webhooks/sms/mtn/status?token=" + testDLRToken,
	}
	if !jsonEqual(s.Body, wantSMS) {
		t.Fatalf("SMS body:\n got %v\nwant %v", s.Body, wantSMS)
	}

	// Ids are printed (the operator must keep them); secrets are not.
	for _, want := range []string{"MTN_USSD_SUBSCRIPTION_ID=123-xyz-456", "MTN_SMS_SUBSCRIPTION_ID=sub-6328f1c2", "token=REDACTED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	assertNoSecrets(t, "subscribe output", out)
}

func TestSubscribeOnlyWhatIsAsked(t *testing.T) {
	fake, requests := subscriptionFake(t)
	cfg := adminConfig(t, fake.URL)
	if out, err := runAdmin(t, cfg, "mtn-subscribe", "-ussd"); err != nil || strings.Contains(out, "SMS:") {
		t.Fatalf("-ussd: %v\n%s", err, out)
	}
	if out, err := runAdmin(t, cfg, "mtn-subscribe", "-sms"); err != nil || strings.Contains(out, "USSD:") {
		t.Fatalf("-sms: %v\n%s", err, out)
	}
	reqs := requests()
	if len(reqs) != 2 || reqs[0].Path != "/v1/messages/ussd/subscription" || reqs[1].Path != "/v1/messages/sms/outbound/SMS-AI/subscription" {
		t.Fatalf("requests: %+v", reqs)
	}
	// Both products can carry both credentials when MTN requires them.
	cfg = adminConfig(t, fake.URL, "MTN_COUNTRY_CODE=GHA", "MTN_API_KEY=separate-api-key", "MTN_TARGET_SYSTEM=pilot",
		"MTN_SMS_SEND_API_KEY=true", "MTN_USSD_SEND_BEARER=true")
	if _, err := runAdmin(t, cfg, "mtn-subscribe"); err != nil {
		t.Fatal(err)
	}
	reqs = requests()
	last := reqs[len(reqs)-1]
	if len(last.Body) != 2 || last.Body["targetSystem"] != "pilot" || last.Header.Get("x-country-code") != "GHA" ||
		last.Header.Get("x-api-key") != "separate-api-key" || last.Header.Get("Authorization") != "Bearer tok-2" ||
		reqs[len(reqs)-2].Header.Get("x-api-key") != "separate-api-key" || reqs[len(reqs)-2].Header.Get("Authorization") != "Bearer tok-2" {
		t.Fatalf("overrides not applied: %+v / %+v", reqs[len(reqs)-2], last)
	}
}

func TestSubscribeDryRunSendsNothing(t *testing.T) {
	fake, requests := subscriptionFake(t)
	out, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-subscribe", "-dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests()) != 0 || fake.tokenCalls.Load() != 0 {
		t.Fatalf("dry run reached MTN: %+v, %d token calls", requests(), fake.tokenCalls.Load())
	}
	if !strings.Contains(out, "Dry run") || !strings.Contains(out, "token=REDACTED") {
		t.Fatalf("output:\n%s", out)
	}
	assertNoSecrets(t, "dry-run output", out)
}

func TestSubscribeRefusesIncompleteConfiguration(t *testing.T) {
	fake, requests := subscriptionFake(t)
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"no callback origin", []string{"MTN_CALLBACK_BASE_URL="}, "MTN_CALLBACK_BASE_URL is required"},
		{"origin with a path", []string{"MTN_CALLBACK_BASE_URL=https://sms.example.com/app"}, "just the origin"},
		{"no USSD token", []string{"MTN_USSD_TOKEN="}, "MTN_USSD_TOKEN is required"},
		{"no delivery token", []string{"MTN_DLR_TOKEN="}, "MTN_DLR_TOKEN is required"},
		{"no client secret", []string{"MTN_CLIENT_SECRET="}, "MTN_CLIENT_ID and MTN_CLIENT_SECRET"},
		{"no client id", []string{"MTN_CLIENT_ID="}, "MTN_API_KEY"},
		{"empty target system", []string{"MTN_TARGET_SYSTEM= "}, ""}, // blank falls back to the default: valid
	}
	for _, c := range cases {
		out, err := runAdmin(t, adminConfig(t, fake.URL, c.extra...), "mtn-subscribe", "-dry-run")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v\n%s", c.name, c.want, err, out)
		}
	}
	// Outside development and test the callback origin must be https.
	cfg := adminConfig(t, "https://sandbox.api.mtn.com", "APP_ENV=staging", "MTN_CALLBACK_BASE_URL=http://sms.example.com")
	if err := cfg.ValidateMTNRegistration(true, true); err == nil || !strings.Contains(err.Error(), "MTN_CALLBACK_BASE_URL: ") || !strings.Contains(err.Error(), "must use https") {
		t.Errorf("plain http origin in staging: %v", err)
	}
	if len(requests()) != 0 {
		t.Fatalf("an incomplete configuration reached MTN: %+v", requests())
	}
}

func TestSubscribeReportsMTNErrorsWithoutSecrets(t *testing.T) {
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"statusCode":"4004","statusMessage":"Service code has already been registered .","supportMessage":"Service code 711 has been used by WEB-ONLINE !.","transactionId":"t1"}`)
	})
	out, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-subscribe")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"HTTP 403", "4004", "already been registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	// Both failures are reported, and nothing secret is.
	if !strings.Contains(err.Error(), "USSD:") || !strings.Contains(err.Error(), "SMS:") {
		t.Errorf("error should name both registrations: %v", err)
	}
	assertNoSecrets(t, "error", err.Error())
	assertNoSecrets(t, "output", out)
	if strings.Contains(out, "_SUBSCRIPTION_ID=") {
		t.Errorf("no id should be printed for a failed registration:\n%s", out)
	}
}

func TestSubscribeRejectsAnIdlessSuccess(t *testing.T) {
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"statusCode":"0000","statusMessage":"ok","data":{}}`)
	})
	if _, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-subscribe", "-ussd"); err == nil || !strings.Contains(err.Error(), "without a subscription id") {
		t.Fatalf("got %v", err)
	}
}

func TestUnsubscribe(t *testing.T) {
	fake, requests := subscriptionFake(t)
	cfg := adminConfig(t, fake.URL, "MTN_USSD_SUBSCRIPTION_ID=123-xyz-456", "MTN_SMS_SUBSCRIPTION_ID=sub-6328f1c2")
	out, err := runAdmin(t, cfg, "mtn-unsubscribe")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	reqs := requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodDelete || reqs[0].Path != "/v1/messages/ussd/subscription/123-xyz-456" ||
		reqs[1].Method != http.MethodDelete || reqs[1].Path != "/v1/messages/sms/outbound/SMS-AI/subscription/sub-6328f1c2" {
		t.Fatalf("requests: %+v", reqs)
	}
	if reqs[0].Header.Get("x-api-key") != testClientID || reqs[1].Header.Get("Authorization") != "Bearer tok-1" ||
		reqs[1].Header.Get("x-country-code") != "GH" || reqs[1].Header.Get("transactionId") == "" {
		t.Fatalf("auth headers: %v / %v", reqs[0].Header, reqs[1].Header)
	}
	assertNoSecrets(t, "unsubscribe output", out)

	// Flags beat the recorded ids, and one side can be removed alone.
	if _, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-unsubscribe", "-ussd-id", "123-xyz-456"); err != nil {
		t.Fatal(err)
	}
	if got := requests(); len(got) != 3 || got[2].Path != "/v1/messages/ussd/subscription/123-xyz-456" {
		t.Fatalf("requests: %+v", got)
	}

	// Dry run and missing ids send nothing.
	n := len(requests())
	if _, err := runAdmin(t, cfg, "mtn-unsubscribe", "-dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-unsubscribe"); err == nil || !strings.Contains(err.Error(), "nothing to remove") {
		t.Fatalf("no ids: %v", err)
	}
	if _, err := runAdmin(t, adminConfig(t, fake.URL, "MTN_CLIENT_SECRET="), "mtn-unsubscribe", "-sms-id", "sub-6328f1c2"); err == nil {
		t.Fatal("removing without credentials should fail")
	}
	if len(requests()) != n {
		t.Fatalf("something was sent: %+v", requests()[n:])
	}
	// An id MTN does not know is an error with MTN's reason.
	if _, err := runAdmin(t, adminConfig(t, fake.URL), "mtn-unsubscribe", "-ussd-id", "nope"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestStatusShowsConfigurationWithoutSecrets(t *testing.T) {
	fake, requests := subscriptionFake(t)
	cfg := adminConfig(t, fake.URL, "PRIMARY_SMS_PROVIDER=mtn", "PRIMARY_USSD_PROVIDER=mtn", "MTN_USSD_SUBSCRIPTION_ID=123-xyz-456")
	out, err := runAdmin(t, cfg, "mtn-status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PRIMARY_SMS_PROVIDER=mtn", "123-xyz-456", "(none)", "token=REDACTED", "client id/secret  set / set", "not an MTN host"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	assertNoSecrets(t, "status output", out)
	if len(requests()) != 0 || fake.tokenCalls.Load() != 0 {
		t.Fatal("status without -check-credentials must not call MTN")
	}

	// The credential check requests one token and prints no part of it.
	out, err = runAdmin(t, cfg, "mtn-status", "-check-credentials")
	if err != nil || !strings.Contains(out, "MTN issued an access token") || fake.tokenCalls.Load() != 1 {
		t.Fatalf("check: %v (%d token calls)\n%s", err, fake.tokenCalls.Load(), out)
	}
	assertNoSecrets(t, "credential-check output", out)

	bad := adminConfig(t, fake.URL, "MTN_CLIENT_SECRET=wrong-secret-value")
	if _, err := runAdmin(t, bad, "mtn-status", "-check-credentials"); err == nil || strings.Contains(err.Error(), "wrong-secret-value") {
		t.Fatalf("a wrong secret should fail without echoing it: %v", err)
	}
	if _, err := runAdmin(t, adminConfig(t, fake.URL, "MTN_CLIENT_SECRET="), "mtn-status", "-check-credentials"); err == nil {
		t.Fatal("check without credentials should fail")
	}
}

func TestRunAdminRejectsUnknownCommandsAndFlags(t *testing.T) {
	cfg := adminConfig(t, "http://127.0.0.1:9")
	if _, err := runAdmin(t, cfg, "mtn-frobnicate"); err == nil {
		t.Fatal("unknown command")
	}
	if _, err := runAdmin(t, cfg, "mtn-subscribe", "-nope"); err == nil {
		t.Fatal("unknown flag")
	}
	if !IsAdminCommand("mtn-subscribe") || !IsAdminCommand("mtn-unsubscribe") || !IsAdminCommand("mtn-status") || IsAdminCommand("usage") {
		t.Fatal("IsAdminCommand")
	}
}
