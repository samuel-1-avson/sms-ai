package mtn

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"smsaighana/internal/channel"
)

const (
	testUSSDToken = "ussd-token-0123456789abcdef0123456789abcdef"
	testDLRToken  = "dlr-token-0123456789abcdef0123456789abcdef"
	testClientID  = "test-client-id"
	testSecret    = "test-client-secret"
	testPhone     = "+233541234567"
	testMsgID     = "f2665231-522f-32b6-accf-6ac8426bfd5c" // 36 characters, like our message ids
	testRequestID = "7f3c1d52-9d1e-4a56-b6a1-0c5e8e2a9b10"
)

var (
	_ channel.USSDGateway = (*Provider)(nil)
	_ channel.Sender      = (*Provider)(nil)
	_ channel.SMSInbound  = (*Provider)(nil)
)

// fixture reads a payload from tests/fixture_webhooks. Their provenance is
// listed in docs/PROVIDER_VALIDATION.md: the names say whether a file is a
// MTN documentation sample, built from Swagger examples, or constructed.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixture_webhooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureMap reads a fixture as a JSON object without its "_note".
func fixtureMap(t testing.TB, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "_note")
	return m
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestProvider(base string) *Provider {
	return New(Options{
		ClientID: testClientID, ClientSecret: testSecret, BaseURL: base, USSDServiceCode: "*713*1#", SenderID: "SMS-AI",
		USSDTokens: []string{testUSSDToken}, DLRTokens: []string{testDLRToken}, Log: quietLog(),
	})
}

func callbackHTTP(method, path, token string, body io.Reader) *http.Request {
	if token != "" {
		path += "?" + TokenParam + "=" + url.QueryEscape(token)
	}
	return httptest.NewRequest(method, path, body)
}

func ussdHTTP(token string) *http.Request {
	return callbackHTTP(http.MethodPost, USSDCallbackPath, token, nil)
}

func dlrHTTP(token, body string) *http.Request {
	return callbackHTTP(http.MethodPost, DLRCallbackPath, token, strings.NewReader(body))
}

// fakeMTN is a stand-in for api.mtn.com: the OAuth token endpoint plus a
// handler for everything else. Tokens are tok-1, tok-2 ... in issue order.
type fakeMTN struct {
	*httptest.Server
	tokenCalls atomic.Int32
	apiCalls   atomic.Int32
}

func newFakeMTN(t testing.TB, api http.HandlerFunc) *fakeMTN {
	t.Helper()
	f := &fakeMTN{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == DefaultTokenPath {
			f.tokenEndpoint(t, w, r)
			return
		}
		f.apiCalls.Add(1)
		api(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// tokenEndpoint checks the request is the one MTN's OAuth page shows: a POST
// with grant_type=client_credentials in the query and the consumer key and
// secret as a form body.
func (f *fakeMTN) tokenEndpoint(t testing.TB, w http.ResponseWriter, r *http.Request) {
	n := f.tokenCalls.Add(1)
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method != http.MethodPost || r.URL.Query().Get("grant_type") != "client_credentials" ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") ||
		form.Get("client_id") != testClientID || form.Get("client_secret") != testSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"statusCode":"4000","statusMessage":"Unauthorised"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"BearerToken","expires_in":"3599","status":"approved"}`, n)
}

func outbound() channel.OutboundSMS {
	return channel.OutboundSMS{
		To: testPhone, SenderID: "SMS-AI", Body: "Photosynthesis turns light into sugar.", Encoding: "GSM-7", ClientRef: testMsgID,
	}
}
