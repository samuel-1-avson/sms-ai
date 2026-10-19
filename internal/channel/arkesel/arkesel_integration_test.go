package arkesel_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/channel/arkesel"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/testenv"
)

const (
	itToken  = "it-ussd-token-0123456789abcdef0123456789"
	itSecret = "it-webhook-secret-0123456789abcdef"
	itPhone  = "233271231234" // the msisdn in Arkesel's USSD example
)

// TestArkeselEndToEnd drives the real HTTP handler and engine with Arkesel's
// documented payloads: a USSD session, an answer saved to SMS, the MT send
// to a fake Arkesel API and a signed delivery report.
func TestArkeselEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var sends []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/sms/send" || r.Header.Get("api-key") != "it-api-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"status":"error","message":"Invalid key"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sends = append(sends, body)
		n := len(sends)
		mu.Unlock()
		fmt.Fprintf(w, `{"status":"success","data":[{"recipient":%q,"id":"it-msg-%d"}]}`, itPhone, n)
	}))
	defer fake.Close()

	dbURL := testenv.FreshDatabase(t, "smsai_it_arkesel")
	environ := append(testenv.BaseEnviron(dbURL),
		"PRIMARY_SMS_PROVIDER=arkesel", "PRIMARY_USSD_PROVIDER=arkesel", "SMS_ENVIRONMENT=sandbox",
		"ARKESEL_API_KEY=it-api-key", "ARKESEL_BASE_URL="+fake.URL,
		"ARKESEL_USSD_TOKEN=REPLACE_ME_LATER,"+itToken, "ARKESEL_WEBHOOK_SECRET="+itSecret,
		"ARKESEL_DLR_CALLBACK_URL=https://api.example.test/webhooks/sms/arkesel/status")
	env := testenv.NewOn(t, dbURL, environ, nil)
	srv := httptest.NewServer(httpapi.NewHandler(env.App))
	defer srv.Close()

	session := readFixture(t, "arkesel_ussd_new_session.json")
	step := func(userData string) map[string]any {
		m := readFixtureMap(t, "arkesel_ussd_continue.json")
		m["userData"] = userData
		b, _ := json.Marshal(m)
		return ussd(t, srv, itToken, b, http.StatusOK)
	}

	// Unsigned USSD callbacks without the shared token are refused.
	ussd(t, srv, "", session, http.StatusUnauthorized)
	ussd(t, srv, "wrong-token-0123456789abcdef", session, http.StatusUnauthorized)

	welcome := ussd(t, srv, itToken, session, http.StatusOK)
	if welcome["sessionID"] != "2005506191900168" || welcome["userID"] != "USSD_DOCUMENTATION" ||
		welcome["msisdn"] != itPhone || welcome["continueSession"] != true {
		t.Fatalf("welcome reply does not echo the request: %v", welcome)
	}
	answer := step("What is photosynthesis?")
	if answer["continueSession"] != true || answer["message"] == "" {
		t.Fatalf("inline answer: %v", answer)
	}
	saved := step("2") // 2.Save to SMS
	if saved["continueSession"] != false {
		t.Fatalf("save to SMS should end the session: %v", saved)
	}

	env.Drain(5 * time.Second)
	mu.Lock()
	got := append([]map[string]any(nil), sends...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want one MT SMS at Arkesel, got %d", len(got))
	}
	if got[0]["sender"] != env.Cfg.SMSOutboundSenderID || got[0]["sandbox"] != true ||
		got[0]["callback_url"] != "https://api.example.test/webhooks/sms/arkesel/status" {
		t.Fatalf("send request: %v", got[0])
	}
	if rc, _ := got[0]["recipients"].([]any); len(rc) != 1 || rc[0] != itPhone {
		t.Fatalf("recipients: %v", got[0]["recipients"])
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'arkesel' AND provider_message_id = 'it-msg-1' AND status = 'submitted'`); n != 1 {
		t.Fatal("submitted message not recorded with Arkesel's id")
	}

	// Delivery report, sent as GET like Arkesel's docs show: unsigned is
	// refused, signed is applied, and a POST of the same report also works.
	query := map[string]string{"sms_id": "it-msg-1", "status": "DELIVERED"}
	if code := dlr(t, srv, http.MethodGet, query, ""); code != http.StatusUnauthorized {
		t.Fatalf("unsigned receipt: %d", code)
	}
	if code := dlr(t, srv, http.MethodGet, query, itSecret); code != http.StatusOK {
		t.Fatalf("signed GET receipt: %d", code)
	}
	if code := dlr(t, srv, http.MethodPost, query, itSecret); code != http.StatusOK {
		t.Fatalf("signed POST receipt: %d", code)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_message_id = 'it-msg-1' AND status = 'delivered'`); n != 1 {
		t.Fatal("delivery report not applied")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixture_webhooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFixtureMap(t *testing.T, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readFixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func ussd(t *testing.T, srv *httptest.Server, token string, body []byte, wantStatus int) map[string]any {
	t.Helper()
	target := srv.URL + "/webhooks/ussd/arkesel/session"
	if token != "" {
		target += "?" + arkesel.TokenParam + "=" + url.QueryEscape(token)
	}
	resp, err := srv.Client().Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("USSD %s: status %d, want %d: %s", body, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func dlr(t *testing.T, srv *httptest.Server, method string, query map[string]string, secret string) int {
	t.Helper()
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	req, _ := http.NewRequest(method, srv.URL+"/webhooks/sms/arkesel/status?"+q.Encode(), nil)
	if secret != "" {
		ts := time.Now().Unix()
		sig, err := arkesel.Sign(secret, ts, query)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(arkesel.HeaderTimestamp, strconv.FormatInt(ts, 10))
		req.Header.Set(arkesel.HeaderSignature, sig)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
