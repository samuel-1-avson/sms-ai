package mtn_test

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
	"smsaighana/internal/channel/mtn"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/testenv"
)

const (
	itUSSDToken = "it-ussd-token-0123456789abcdef0123456789"
	itDLRToken  = "it-dlr-token-0123456789abcdef0123456789"
	itPhone     = "233541234567" // the msisdn of the constructed USSD fixtures
	itRequestID = "7f3c1d52-9d1e-4a56-b6a1-0c5e8e2a9b10"
)

// fakeMTN stands in for api.mtn.com: the OAuth token endpoint and the SMS V2
// send endpoint. Nothing here talks to the real platform.
type fakeMTN struct {
	*httptest.Server
	mu          sync.Mutex
	sends       []map[string]any
	tokens      int
	sendCode    int // 0 = success; else the HTTP status to answer with
	lookupFound bool
	lookupCalls int
}

func newFakeMTN(t *testing.T) *fakeMTN {
	f := &fakeMTN{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/oauth/access_token":
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			if r.Method != http.MethodPost || r.URL.Query().Get("grant_type") != "client_credentials" ||
				form.Get("client_id") != "it-client-id" || form.Get("client_secret") != "it-client-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"statusCode":"4000","statusMessage":"Unauthorised"}`)
				return
			}
			f.tokens++
			fmt.Fprintf(w, `{"access_token":"it-token-%d","token_type":"BearerToken","expires_in":"3599"}`, f.tokens)
		case "/v1/messages/sms/outbound":
			if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer it-token-") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"statusCode":"4000","statusMessage":"Unauthorised"}`)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("transactionId") != body["clientCorrelator"] {
				t.Error("send transactionId must match clientCorrelator")
			}
			f.sends = append(f.sends, body)
			if f.sendCode != 0 {
				w.WriteHeader(f.sendCode)
				_, _ = io.WriteString(w, `{"statusCode":"3001","statusMessage":"System error"}`)
				return
			}
			_, _ = w.Write(readFixture(t, "mtn_sms_send_response_constructed.json"))
		default:
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deliveryStatus") && len(f.sends) > 0 {
				f.lookupCalls++
				ref := f.sends[0]["clientCorrelator"].(string)
				if r.URL.Path != "/v1/messages/sms/outbound/SMS-AI/"+ref+"/deliveryStatus" {
					t.Errorf("lookup by client reference: %s", r.URL.Path)
				}
				if f.lookupFound {
					body := readFixtureMap(t, "mtn_sms_delivery_status_response_constructed.json")
					body["data"].(map[string]any)["clientCorrelator"] = ref
					_ = json.NewEncoder(w).Encode(body)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMTN) tokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

func (f *fakeMTN) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sends...)
}

func mtnEnv(t *testing.T, dbName string, fake *fakeMTN, extra ...string) (*testenv.Env, *httptest.Server) {
	t.Helper()
	dbURL := testenv.FreshDatabase(t, dbName)
	environ := append(testenv.BaseEnviron(dbURL),
		"PRIMARY_SMS_PROVIDER=mtn", "PRIMARY_USSD_PROVIDER=mtn", "SMS_ENVIRONMENT=sandbox",
		"MTN_CLIENT_ID=it-client-id", "MTN_CLIENT_SECRET=it-client-secret", "MTN_BASE_URL="+fake.URL,
		"MTN_USSD_TOKEN=REPLACE_ME_LATER,"+itUSSDToken, "MTN_DLR_TOKEN="+itDLRToken)
	env := testenv.NewOn(t, dbURL, append(environ, extra...), nil)
	srv := httptest.NewServer(httpapi.NewHandler(env.App))
	t.Cleanup(srv.Close)
	return env, srv
}

// TestMTNEndToEnd drives the real HTTP handler and engine with MTN-shaped USSD
// requests: a session, an answer saved to SMS, the MT send to a fake MTN API
// (OAuth token included) and delivery notifications.
func TestMTNEndToEnd(t *testing.T) {
	fake := newFakeMTN(t)
	env, srv := mtnEnv(t, "smsai_it_mtn", fake)

	// Selecting MTN replaces the mock gateway: it is not loaded alongside it,
	// and nothing of it is reachable.
	if env.MockGW != nil {
		t.Fatal("the mock gateway must not be loaded when MTN is selected")
	}
	if _, ok := env.Registry.USSD("mock"); ok {
		t.Fatal("a mock USSD gateway is registered")
	}
	if _, ok := env.Registry.SMSInbound("mock"); ok {
		t.Fatal("a mock SMS provider is registered")
	}
	if s, err := env.Registry.SenderFor("mock"); err != nil || s.Name() != mtn.Name {
		t.Fatalf("the only sender must be MTN: %v %v", s, err)
	}
	for _, path := range []string{"/simulator", "/admin/simulate/outbox"} {
		if resp, err := srv.Client().Get(srv.URL + path); err != nil || resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("simulator route %s is reachable: %v %v", path, resp, err)
		}
	}
	if resp, err := srv.Client().Post(srv.URL+"/webhooks/ussd/mock/session", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mock USSD route: %v %v", resp, err)
	}

	begin := readFixture(t, "mtn_ussd_begin_constructed.json")
	step := func(input string) map[string]any {
		m := readFixtureMap(t, "mtn_ussd_continue_constructed.json")
		m["ussdString"] = input
		b, _ := json.Marshal(m)
		return ussd(t, srv, itUSSDToken, b, http.StatusOK)
	}

	// Unauthenticated callbacks and wrong methods are refused.
	ussd(t, srv, "", begin, http.StatusUnauthorized)
	ussd(t, srv, "wrong-token-0123456789abcdef", begin, http.StatusUnauthorized)
	ussd(t, srv, itDLRToken, begin, http.StatusUnauthorized)
	if resp, err := srv.Client().Get(srv.URL + mtn.USSDCallbackPath + "?token=" + itUSSDToken); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the USSD route: %v %v", resp, err)
	}
	ussd(t, srv, itUSSDToken, []byte(`{"sessionId":"x"}`), http.StatusBadRequest)

	welcome := ussd(t, srv, itUSSDToken, begin, http.StatusOK)
	wd := data(t, welcome)
	if welcome["statusCode"] != "0000" || wd["messageType"] != float64(1) || wd["userInputRequired"] != true ||
		wd["msisdn"] != itPhone || !strings.Contains(fmt.Sprint(wd["inboundResponse"]), "Type your question") {
		t.Fatalf("welcome reply: %v", welcome)
	}
	answer := data(t, step("What is photosynthesis?"))
	if answer["messageType"] != float64(1) || answer["inboundResponse"] == "" {
		t.Fatalf("inline answer: %v", answer)
	}
	saved := data(t, step("2")) // 2.Save to SMS
	if saved["messageType"] != float64(2) || saved["userInputRequired"] != false {
		t.Fatalf("save to SMS should end the session: %v", saved)
	}

	env.Drain(5 * time.Second)
	got := fake.sent()
	if len(got) != 1 {
		t.Fatalf("want one MT SMS at MTN, got %d", len(got))
	}
	ref, _ := got[0]["clientCorrelator"].(string)
	receivers, _ := got[0]["receiverAddress"].([]any)
	if got[0]["senderAddress"] != env.Cfg.SMSOutboundSenderID || len(got[0]) != 4 ||
		len(receivers) != 1 || receivers[0] != itPhone ||
		got[0]["message"] == "" || len(ref) != 36 {
		t.Fatalf("send request: %v", got[0])
	}
	if n := fake.tokenRequests(); n != 1 {
		t.Fatalf("the OAuth token should be requested once and cached, got %d requests", n)
	}
	// MTN assigns requestId; both identifiers can appear in notifications.
	requestID := itRequestID
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mtn' AND provider_message_id = $1 AND status = 'submitted'`, requestID); n != 1 {
		t.Fatal("submitted message not recorded under MTN's requestId")
	}

	// Delivery notifications: unauthenticated or with the USSD token are
	// refused; the delivery token works.
	report := readFixtureMap(t, "mtn_sms_delivery_notification_constructed.json")
	report["clientCorrelator"] = ref
	body, _ := json.Marshal(report)
	if code := dlr(t, srv, http.MethodPost, "", body); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated notification: %d", code)
	}
	if code := dlr(t, srv, http.MethodPost, itUSSDToken, body); code != http.StatusUnauthorized {
		t.Fatalf("notification with the USSD token: %d", code)
	}
	delivered := `SELECT count(*) FROM messages WHERE provider_message_id = $1 AND status = 'delivered'`
	if n := env.Count(delivered, requestID); n != 0 {
		t.Fatal("a refused notification was applied")
	}
	if code := dlr(t, srv, http.MethodPost, itDLRToken, []byte(`{"nothing":"useful"}`)); code != http.StatusBadRequest {
		t.Fatalf("notification without a correlator id: %d", code)
	}
	if code := dlr(t, srv, http.MethodPost, itDLRToken, body); code != http.StatusOK {
		t.Fatalf("notification: %d", code)
	}
	if n := env.Count(delivered, requestID); n != 1 {
		t.Fatal("delivery notification not applied")
	}
	// A late ENROUTE, or one for an unknown message, must not break anything or
	// move a delivered message backwards.
	for _, late := range []string{
		`{"requestId":"` + requestID + `","deliveryStatus":[{"status":"ENROUTE"}]}`,
		`{"requestId":"` + requestID + `","deliveryStatus":[{"status":"UNDELIVERED"}]}`,
		`{"requestId":"00000000-0000-0000-0000-000000000000","deliveryStatus":[{"status":"DELIVERED"}]}`,
	} {
		if code := dlr(t, srv, http.MethodPost, itDLRToken, []byte(late)); code != http.StatusOK {
			t.Fatalf("late notification %s: %d", late, code)
		}
	}
	if n := env.Count(delivered, requestID); n != 1 {
		t.Fatal("a late notification moved a delivered message backwards")
	}

	// End, Cancel and Timeout requests close a session without being read as a
	// question.
	questions := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mtn' AND kind = 'question'`)
	for _, name := range []string{"mtn_ussd_end_constructed.json", "mtn_ussd_timeout_constructed.json"} {
		ussd(t, srv, itUSSDToken, begin, http.StatusOK) // a live session to end
		bye := ussd(t, srv, itUSSDToken, readFixture(t, name), http.StatusOK)
		if d := data(t, bye); d["messageType"] != float64(2) || d["msisdn"] != itPhone {
			t.Fatalf("%s: %v", name, bye)
		}
	}
	if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mtn' AND kind = 'question'`); n != questions {
		t.Fatalf("an end or timeout request was booked as a question (%d -> %d)", questions, n)
	}
}

// TestMTNUncertainSendIsNotResent: an MTN error after the request left may
// mean the message was queued and billed. With client-reference lookup disabled it
// waits for an operator instead of being resent.
func TestMTNUncertainSendIsNotResent(t *testing.T) {
	for _, c := range []struct {
		name           string
		enabled, found bool
	}{
		{"default", false, false},
		{"lookup fails", true, false},
		{"lookup confirms submission", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeMTN(t)
			fake.sendCode = http.StatusBadGateway
			fake.lookupFound = c.found
			env, srv := mtnEnv(t, "smsai_it_mtn_uncertain", fake, fmt.Sprintf("MTN_SMS_LOOKUP_BY_CLIENT_REF=%v", c.enabled))
			sender, err := env.Registry.SenderFor(mtn.Name)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := sender.(channel.StatusQuerier); ok != c.enabled {
				t.Fatalf("app wiring: StatusQuerier=%v, enabled=%v", ok, c.enabled)
			}

			begin := readFixture(t, "mtn_ussd_begin_constructed.json")
			ussd(t, srv, itUSSDToken, begin, http.StatusOK)
			for _, input := range []string{"What is photosynthesis?", "2"} {
				m := readFixtureMap(t, "mtn_ussd_continue_constructed.json")
				m["ussdString"] = input
				b, _ := json.Marshal(m)
				ussd(t, srv, itUSSDToken, b, http.StatusOK)
			}
			env.Drain(5 * time.Second)
			if n := len(fake.sent()); n != 1 {
				t.Fatalf("want one send attempt, got %d", n)
			}
			unknown := `SELECT count(*) FROM messages WHERE provider_name = 'mtn' AND status = 'unknown_timeout'`
			if n := env.Count(unknown); n != 1 {
				t.Fatal("the message should be marked unknown_timeout")
			}
			env.Engine.ReconcileUncertainSends(context.Background())
			env.Drain(2 * time.Second)
			if n := len(fake.sent()); n != 1 {
				t.Fatalf("the reconciler resent an uncertain message (%d sends)", n)
			}
			if c.found {
				if n := env.Count(`SELECT count(*) FROM messages WHERE provider_name = 'mtn' AND status = 'submitted' AND provider_message_id = $1`, itRequestID); n != 1 {
					t.Fatal("confirmed send was not recorded under MTN's requestId")
				}
			} else if n := env.Count(unknown); n != 1 {
				t.Fatal("a failed/disabled lookup changed an uncertain message")
			}
			fake.mu.Lock()
			queries := fake.lookupCalls
			fake.mu.Unlock()
			if c.enabled && queries != 1 || !c.enabled && queries != 0 {
				t.Fatalf("lookup requests=%d, enabled=%v", queries, c.enabled)
			}
		})
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
	return m
}

func data(t testing.TB, reply map[string]any) map[string]any {
	t.Helper()
	d, ok := reply["data"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no data object: %v", reply)
	}
	return d
}

func ussd(t testing.TB, srv *httptest.Server, token string, body []byte, wantStatus int) map[string]any {
	t.Helper()
	target := srv.URL + mtn.USSDCallbackPath
	if token != "" {
		target += "?" + mtn.TokenParam + "=" + url.QueryEscape(token)
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

// dlr posts a delivery notification as JSON.
func dlr(t testing.TB, srv *httptest.Server, method, token string, body []byte) int {
	t.Helper()
	target := srv.URL + mtn.DLRCallbackPath
	if token != "" {
		target += "?" + mtn.TokenParam + "=" + url.QueryEscape(token)
	}
	req, _ := http.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
