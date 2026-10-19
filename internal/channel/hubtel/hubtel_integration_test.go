package hubtel_test

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
	"strings"
	"sync"
	"testing"
	"time"

	"smsaighana/internal/channel/hubtel"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/testenv"
)

const (
	itUSSDToken = "it-ussd-token-0123456789abcdef0123456789"
	itDLRToken  = "it-dlr-token-0123456789abcdef0123456789"
	itPhone     = "233208074263" // the mobile in Hubtel's USSD sample
)

// TestHubtelEndToEnd drives the real HTTP handler and engine with Hubtel's
// documented USSD requests: a session, an answer saved to SMS, the MT send to
// a fake Hubtel API and delivery reports.
func TestHubtelEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var sends []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" || !ok || user != "it-client-id" || pass != "it-client-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"Message":"Unauthorized"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sends = append(sends, body)
		n := len(sends)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Status":0,"MessageId":"it-msg-%d","Rate":1,"NetworkId":"62002","ClientReference":%q}`, n, body["ClientReference"])
	}))
	defer fake.Close()

	dbURL := testenv.FreshDatabase(t, "smsai_it_hubtel")
	environ := append(testenv.BaseEnviron(dbURL),
		"PRIMARY_SMS_PROVIDER=hubtel", "PRIMARY_USSD_PROVIDER=hubtel", "SMS_ENVIRONMENT=live",
		"HUBTEL_CLIENT_ID=it-client-id", "HUBTEL_CLIENT_SECRET=it-client-secret", "HUBTEL_SMS_BASE_URL="+fake.URL,
		"HUBTEL_USSD_TOKEN=REPLACE_ME_LATER,"+itUSSDToken, "HUBTEL_DLR_TOKEN="+itDLRToken)
	env := testenv.NewOn(t, dbURL, environ, nil)
	// Selecting a real provider replaces the mock gateway: it is not loaded
	// alongside it.
	if env.MockGW != nil {
		t.Fatal("the mock gateway must not be loaded when Hubtel is selected")
	}
	srv := httptest.NewServer(httpapi.NewHandler(env.App))
	defer srv.Close()

	initiation := readFixture(t, "hubtel_ussd_initiation.json")
	step := func(message string) map[string]any {
		m := readFixtureMap(t, "hubtel_ussd_response.json")
		m["message"] = message
		b, _ := json.Marshal(m)
		return ussd(t, srv, itUSSDToken, b, http.StatusOK)
	}

	// Unauthenticated callbacks and wrong methods are refused.
	ussd(t, srv, "", initiation, http.StatusUnauthorized)
	ussd(t, srv, "wrong-token-0123456789abcdef", initiation, http.StatusUnauthorized)
	ussd(t, srv, itDLRToken, initiation, http.StatusUnauthorized)
	if resp, err := srv.Client().Get(srv.URL + "/webhooks/ussd/hubtel/session?token=" + itUSSDToken); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the USSD route: %v %v", resp, err)
	}

	welcome := ussd(t, srv, itUSSDToken, initiation, http.StatusOK)
	if welcome["SessionId"] != "3c796dac28174f739de4262d08409c51" || welcome["type"] != "Response" ||
		!strings.Contains(fmt.Sprint(welcome["message"]), "Type your question") {
		t.Fatalf("welcome reply: %v", welcome)
	}
	answer := step("What is photosynthesis?")
	if answer["type"] != "Response" || answer["message"] == "" {
		t.Fatalf("inline answer: %v", answer)
	}
	saved := step("2") // 2.Save to SMS
	if saved["type"] != "Release" {
		t.Fatalf("save to SMS should end the session: %v", saved)
	}

	env.Drain(5 * time.Second)
	mu.Lock()
	got := append([]map[string]any(nil), sends...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want one MT SMS at Hubtel, got %d", len(got))
	}
	if got[0]["From"] != env.Cfg.SMSOutboundSenderID || got[0]["To"] != itPhone || got[0]["RegisteredDelivery"] != true ||
		got[0]["Content"] == "" || got[0]["ClientReference"] == "" || got[0]["ClientReference"] == nil {
		t.Fatalf("send request: %v", got[0])
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'hubtel' AND provider_message_id = 'it-msg-1' AND status = 'submitted'`); n != 1 {
		t.Fatal("submitted message not recorded with Hubtel's id")
	}

	// Delivery reports: unauthenticated or with the USSD token are refused;
	// the delivery-report token works for a JSON body and for a query string.
	report := readFixtureMap(t, "hubtel_sms_status_callback_assumed.json")
	report["MessageId"] = "it-msg-1"
	body, _ := json.Marshal(report)
	if code := dlr(t, srv, http.MethodPost, "", "", body); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated receipt: %d", code)
	}
	if code := dlr(t, srv, http.MethodPost, itUSSDToken, "", body); code != http.StatusUnauthorized {
		t.Fatalf("receipt with the USSD token: %d", code)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_message_id = 'it-msg-1' AND status = 'delivered'`); n != 0 {
		t.Fatal("a refused receipt was applied")
	}
	if code := dlr(t, srv, http.MethodPost, itDLRToken, "", body); code != http.StatusOK {
		t.Fatalf("JSON receipt: %d", code)
	}
	if code := dlr(t, srv, http.MethodGet, itDLRToken, "MessageId=it-msg-1&Status=Delivered", nil); code != http.StatusOK {
		t.Fatalf("query receipt: %d", code)
	}
	if code := dlr(t, srv, http.MethodPost, itDLRToken, "", []byte(`{"nothing":"useful"}`)); code != http.StatusBadRequest {
		t.Fatalf("receipt without a message id: %d", code)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_message_id = 'it-msg-1' AND status = 'delivered'`); n != 1 {
		t.Fatal("delivery report not applied")
	}
	// A late "Sent" must not move a delivered message backwards.
	if code := dlr(t, srv, http.MethodPost, itDLRToken, "", []byte(`{"MessageId":"it-msg-1","Status":"Sent"}`)); code != http.StatusOK {
		t.Fatalf("late receipt: %d", code)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_message_id = 'it-msg-1' AND status = 'delivered'`); n != 1 {
		t.Fatal("a late receipt moved a delivered message backwards")
	}

	// Release and Timeout requests end a session without being read as a question.
	questions := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'hubtel' AND kind = 'question'`)
	for _, name := range []string{"hubtel_ussd_release_constructed.json", "hubtel_ussd_timeout_constructed.json"} {
		ussd(t, srv, itUSSDToken, initiation, http.StatusOK) // a live session to end
		bye := ussd(t, srv, itUSSDToken, readFixture(t, name), http.StatusOK)
		if bye["type"] != "Release" || bye["SessionId"] != "3c796dac28174f739de4262d08409c51" {
			t.Fatalf("%s: %v", name, bye)
		}
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'hubtel' AND kind = 'question'`); n != questions {
		t.Fatalf("a release or timeout request was booked as a question (%d -> %d)", questions, n)
	}

	// The SDK-style initiation (PascalCase keys, dialled code as the message)
	// opens a fresh session for another number.
	other := ussd(t, srv, itUSSDToken, readFixture(t, "hubtel_ussd_initiation_sdk.json"), http.StatusOK)
	if other["SessionId"] != "234567ityjhgwe654765utrye" || other["type"] != "Response" {
		t.Fatalf("SDK-style initiation: %v", other)
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
	target := srv.URL + "/webhooks/ussd/hubtel/session"
	if token != "" {
		target += "?" + hubtel.TokenParam + "=" + url.QueryEscape(token)
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

// dlr sends a delivery report. query is added after the token; a body is
// sent as JSON.
func dlr(t *testing.T, srv *httptest.Server, method, token, query string, body []byte) int {
	t.Helper()
	target := srv.URL + "/webhooks/sms/hubtel/status"
	var parts []string
	if token != "" {
		parts = append(parts, hubtel.TokenParam+"="+url.QueryEscape(token))
	}
	if query != "" {
		parts = append(parts, query)
	}
	if len(parts) > 0 {
		target += "?" + strings.Join(parts, "&")
	}
	req, _ := http.NewRequest(method, target, bytes.NewReader(body))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
