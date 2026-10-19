package mnotify_test

import (
	"bytes"
	"context"
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

	"smsaighana/internal/channel"
	"smsaighana/internal/channel/mnotify"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/testenv"
)

const (
	itToken = "it-ussd-token-0123456789abcdef0123456789"
	itKey   = "it-api-key+0123/456=789abcdef"
	itPhone = "233541509394" // the msisdn of mNotify's USSD examples
	itLocal = "0541509394"   // the same number in the local form SMS recipients use
)

// fakeMNotify stands in for api.mnotify.com: the Quick SMS endpoint, which
// records what it was sent and answers with the campaign id of the OpenAPI
// example's shape. Nothing here talks to the real platform.
type fakeMNotify struct {
	*httptest.Server
	mu    sync.Mutex
	sends []map[string]any
	// mode decides the answer: "" = success, "502" = a gateway error after the
	// request arrived, "drop" = the connection is cut, "sender" = a rejection
	// for an unregistered sender ID.
	mode string
}

func newFakeMNotify(t *testing.T) *fakeMNotify {
	f := &fakeMNotify{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/sms/quick" || r.URL.Query().Get("key") != itKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"status":"error","message":"Unauthenticated."}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sends = append(f.sends, body)
		switch f.mode {
		case "502":
			w.WriteHeader(http.StatusBadGateway)
		case "drop":
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		case "sender":
			_, _ = io.WriteString(w, `{"status":"error","code":"4000","message":"Sender ID is not registered"}`)
		default:
			var resp map[string]any
			_ = json.Unmarshal(readFixture(t, "mnotify_sms_quick_response_openapi_example.json"), &resp)
			resp["summary"].(map[string]any)["_id"] = fmt.Sprintf("it-campaign-%d", len(f.sends))
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMNotify) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sends...)
}

func mnotifyEnv(t *testing.T, dbName string, fake *fakeMNotify, extra ...string) (*testenv.Env, *httptest.Server) {
	t.Helper()
	dbURL := testenv.FreshDatabase(t, dbName)
	environ := append(testenv.BaseEnviron(dbURL),
		"PRIMARY_SMS_PROVIDER=mnotify", "PRIMARY_USSD_PROVIDER=mnotify", "SMS_ENVIRONMENT=live",
		"MNOTIFY_API_KEY="+itKey, "MNOTIFY_BASE_URL="+fake.URL+"/api",
		"MNOTIFY_USSD_TOKEN=REPLACE_ME_LATER,"+itToken)
	env := testenv.NewOn(t, dbURL, append(environ, extra...), nil)
	srv := httptest.NewServer(httpapi.NewHandler(env.App))
	t.Cleanup(srv.Close)
	return env, srv
}

// TestMNotifyEndToEnd drives the real HTTP handler and engine with mNotify's
// documented USSD requests in both modes (shared and dedicated): a session, an
// answer saved to SMS, and the MT send to a fake mNotify API.
func TestMNotifyEndToEnd(t *testing.T) {
	fake := newFakeMNotify(t)
	env, srv := mnotifyEnv(t, "smsai_it_mnotify", fake)

	// Selecting mNotify replaces the mock gateway: it is not loaded alongside
	// it, and nothing of it is reachable.
	if env.MockGW != nil {
		t.Fatal("the mock gateway must not be loaded when mNotify is selected")
	}
	if _, ok := env.Registry.USSD("mock"); ok {
		t.Fatal("a mock USSD gateway is registered")
	}
	if _, ok := env.Registry.SMSInbound("mock"); ok {
		t.Fatal("a mock SMS provider is registered")
	}
	if s, err := env.Registry.SenderFor("mock"); err != nil || s.Name() != mnotify.Name {
		t.Fatalf("the only sender must be mNotify: %v %v", s, err)
	}
	for _, path := range []string{"/simulator", "/admin/simulate/outbox"} {
		if resp, err := srv.Client().Get(srv.URL + path); err != nil || resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("simulator route %s is reachable: %v %v", path, resp, err)
		}
	}
	if resp, err := srv.Client().Post(srv.URL+"/webhooks/ussd/mock/session", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mock USSD route: %v %v", resp, err)
	}
	// The provider is no StatusQuerier: an uncertain send is never looked up.
	sender, err := env.Registry.SenderFor(mnotify.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sender.(channel.StatusQuerier); ok {
		t.Fatal("mNotify must not be a StatusQuerier")
	}

	// Callbacks without the token, with a wrong one, with another method, or
	// without a usable payload are refused.
	begin := readFixture(t, "mnotify_ussd_shared_request_openapi_example.json")
	ussd(t, srv, "", begin, http.StatusUnauthorized)
	ussd(t, srv, "wrong-token-0123456789abcdef", begin, http.StatusUnauthorized)
	ussd(t, srv, itKey, begin, http.StatusUnauthorized) // the API key is not a callback credential
	ussd(t, srv, itToken, []byte(`{"msisdn":"x"}`), http.StatusBadRequest)
	ussd(t, srv, itToken, []byte(`nope`), http.StatusBadRequest)
	if resp, err := srv.Client().Get(srv.URL + mnotify.USSDCallbackPath + "?token=" + itToken); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the USSD route: %v %v", resp, err)
	}
	// mNotify documents no webhooks other than USSD: SMS inbound and delivery
	// reports are refused even with the token.
	for _, path := range []string{"/webhooks/sms/mnotify/inbound", "/webhooks/sms/mnotify/status"} {
		resp, err := srv.Client().Post(srv.URL+path+"?token="+itToken, "application/json", strings.NewReader(`{"status":"success","report":[{"_id":1,"status":"DELIVERED"}]}`))
		if err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST %s: %v %v", path, resp, err)
		}
	}

	// --- Shared mode ---
	step := func(input string) map[string]any {
		m := readFixtureMap(t, "mnotify_ussd_shared_continue_constructed.json")
		m["data"] = input
		b, _ := json.Marshal(m)
		return ussd(t, srv, itToken, b, http.StatusOK)
	}
	welcome := ussd(t, srv, itToken, begin, http.StatusOK)
	wmsg := fmt.Sprint(welcome["message"])
	if welcome["msisdn"] != itPhone || welcome["sequenceID"] != "6789940010" || welcome["continueFlag"] != float64(0) ||
		!strings.Contains(wmsg, "Type your question") || !strings.Contains(wmsg, "\r\n") || strings.Contains(strings.ReplaceAll(wmsg, "\r\n", ""), "\n") {
		t.Fatalf("welcome reply: %v", welcome)
	}
	answer := step("What is photosynthesis?")
	if answer["continueFlag"] != float64(0) || fmt.Sprint(answer["message"]) == "" || answer["sequenceID"] != "6789940011" {
		t.Fatalf("inline answer: %v", answer)
	}
	saved := step("2") // 2.Save to SMS
	if saved["continueFlag"] != float64(1) {
		t.Fatalf("save to SMS should end the session: %v", saved)
	}

	env.Drain(5 * time.Second)
	got := fake.sent()
	if len(got) != 1 {
		t.Fatalf("want one MT SMS at mNotify, got %d", len(got))
	}
	recipients, _ := got[0]["recipient"].([]any)
	if got[0]["sender"] != env.Cfg.SMSOutboundSenderID || len(recipients) != 1 || recipients[0] != itLocal ||
		got[0]["message"] == "" || got[0]["is_schedule"] != false || got[0]["schedule_date"] != "" {
		t.Fatalf("send request: %v", got[0])
	}
	if _, has := got[0]["sms_type"]; has || len(got[0]) != 5 {
		t.Fatalf("the request must not carry sms_type or other extras: %v", got[0])
	}
	// The campaign id is recorded as the provider message id. Delivery stays
	// 'submitted': mNotify documents no delivery-report webhook.
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mnotify' AND provider_message_id = 'it-campaign-1' AND status = 'submitted'`); n != 1 {
		t.Fatal("submitted message not recorded with mNotify's campaign id")
	}

	// A request after our END starts a new session, even without a dial string:
	// it must not be read as a menu choice of the finished one.
	again := step("1")
	if !strings.Contains(fmt.Sprint(again["message"]), "Type your question") || again["continueFlag"] != float64(0) {
		t.Fatalf("a request after the END should open a new session: %v", again)
	}

	// --- Dedicated mode: same route, the other payload shape ---
	dstep := func(text string) map[string]any {
		m := readFixtureMap(t, "mnotify_ussd_dedicated_request_openapi_example.json")
		m["sessId"], m["text"] = "SESS-0001", text
		b, _ := json.Marshal(m)
		return ussd(t, srv, itToken, b, http.StatusOK)
	}
	first := ussd(t, srv, itToken, readFixture(t, "mnotify_ussd_dedicated_begin_constructed.json"), http.StatusOK)
	if first["responseExitCode"] != float64(200) || first["shouldClose"] != false || !strings.Contains(fmt.Sprint(first["ussdMenu"]), "Type your question") {
		t.Fatalf("dedicated welcome: %v", first)
	}
	if menu := fmt.Sprint(first["ussdMenu"]); !strings.Contains(menu, "\r\n") {
		t.Fatalf("dedicated menu must use \\r\\n: %q", menu)
	}
	if a := dstep("What is photosynthesis?"); a["shouldClose"] != false || !strings.Contains(fmt.Sprint(a["ussdMenu"]), "2.Save to SMS") {
		t.Fatalf("dedicated inline answer: %v", a)
	}
	if end := dstep("2"); end["shouldClose"] != true {
		t.Fatalf("dedicated save to SMS should close the session: %v", end)
	}
	env.Drain(5 * time.Second)
	if n := len(fake.sent()); n != 2 {
		t.Fatalf("want a second MT SMS after the dedicated session, got %d sends", n)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mnotify' AND status = 'submitted'`); n != 2 {
		t.Fatalf("%d submitted messages, want 2", n)
	}
}

// TestMNotifyFixedModes: with MNOTIFY_USSD_MODE pinned, the other payload
// shape is refused by the real handler.
func TestMNotifyFixedModes(t *testing.T) {
	for _, c := range []struct{ mode, ok, bad string }{
		{"shared", "mnotify_ussd_shared_request_openapi_example.json", "mnotify_ussd_dedicated_request_openapi_example.json"},
		{"dedicated", "mnotify_ussd_dedicated_request_openapi_example.json", "mnotify_ussd_shared_request_openapi_example.json"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			_, srv := mnotifyEnv(t, "smsai_it_mnotify_"+c.mode, newFakeMNotify(t), "MNOTIFY_USSD_MODE="+c.mode)
			ussd(t, srv, itToken, readFixture(t, c.ok), http.StatusOK)
			ussd(t, srv, itToken, readFixture(t, c.bad), http.StatusBadRequest)
		})
	}
}

// TestMNotifyUncertainSendIsNotResent: an mNotify error after the request left
// may mean the message was queued and billed, and a send has no reference to
// look it up by. It waits for an operator instead of being resent, however the
// reconciler runs.
func TestMNotifyUncertainSendIsNotResent(t *testing.T) {
	for _, mode := range []string{"502", "drop"} {
		t.Run(mode, func(t *testing.T) {
			fake := newFakeMNotify(t)
			fake.mode = mode
			env, srv := mnotifyEnv(t, "smsai_it_mnotify_uncertain", fake)

			ussd(t, srv, itToken, readFixture(t, "mnotify_ussd_shared_request_openapi_example.json"), http.StatusOK)
			for _, input := range []string{"What is photosynthesis?", "2"} {
				m := readFixtureMap(t, "mnotify_ussd_shared_continue_constructed.json")
				m["data"] = input
				b, _ := json.Marshal(m)
				ussd(t, srv, itToken, b, http.StatusOK)
			}
			env.Drain(5 * time.Second)
			if n := len(fake.sent()); n != 1 {
				t.Fatalf("want one send attempt, got %d", n)
			}
			unknown := `SELECT count(*) FROM messages WHERE provider_name = 'mnotify' AND status = 'unknown_timeout'`
			if n := env.Count(unknown); n != 1 {
				t.Fatal("the message should be marked unknown_timeout")
			}
			env.Engine.ReconcileUncertainSends(context.Background())
			env.Drain(2 * time.Second)
			if n := len(fake.sent()); n != 1 {
				t.Fatalf("the reconciler resent an uncertain message (%d sends)", n)
			}
			if n := env.Count(unknown); n != 1 {
				t.Fatal("reconciling changed an uncertain message")
			}
			// The failure text kept for the operator names no key.
			var detail string
			if err := env.Store.Pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(error, ' | '), '') FROM send_attempts`).Scan(&detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, itKey) || strings.Contains(detail, url.QueryEscape(itKey)) {
				t.Fatalf("send_attempts holds the API key: %s", detail)
			}
		})
	}
}

// TestMNotifyRejectionIsFinal: a rejection of the message itself (here an
// unregistered sender ID) is not retried and not parked.
func TestMNotifyRejectionIsFinal(t *testing.T) {
	fake := newFakeMNotify(t)
	fake.mode = "sender"
	env, srv := mnotifyEnv(t, "smsai_it_mnotify_rejected", fake)
	ussd(t, srv, itToken, readFixture(t, "mnotify_ussd_shared_request_openapi_example.json"), http.StatusOK)
	for _, input := range []string{"What is photosynthesis?", "2"} {
		m := readFixtureMap(t, "mnotify_ussd_shared_continue_constructed.json")
		m["data"] = input
		b, _ := json.Marshal(m)
		ussd(t, srv, itToken, b, http.StatusOK)
	}
	env.Drain(5 * time.Second)
	if n := len(fake.sent()); n != 1 {
		t.Fatalf("a permanent rejection must not be retried: %d sends", n)
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mnotify' AND status = 'failed'`); n != 1 {
		t.Fatal("the rejected message should be failed")
	}
}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixture_webhooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFixtureMap(t testing.TB, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readFixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "_note")
	return m
}

func ussd(t testing.TB, srv *httptest.Server, token string, body []byte, wantStatus int) map[string]any {
	t.Helper()
	target := srv.URL + mnotify.USSDCallbackPath
	if token != "" {
		target += "?" + mnotify.TokenParam + "=" + url.QueryEscape(token)
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
