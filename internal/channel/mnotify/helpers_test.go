package mnotify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"smsaighana/internal/channel"
)

const (
	testUSSDToken = "ussd-token-0123456789abcdef0123456789abcdef"
	// A base64-looking key: its URL-escaped form differs from the raw one, so a
	// scrub that only handles one of them fails the tests.
	testKey        = "k3y+ABC/def=GHIJKL0123456789"
	testPhone      = "+233541234567"
	testMsgID      = "f2665231-522f-32b6-accf-6ac8426bfd5c" // 36 characters, like our message ids
	testCampaignID = "A59CCB70-662D-45EF-9976-1EFAD249793D" // the _id of the OpenAPI example
)

var (
	_ channel.USSDGateway = (*Provider)(nil)
	_ channel.Sender      = (*Provider)(nil)
	_ channel.SMSInbound  = (*Provider)(nil)
)

// fixture reads a payload from tests/fixture_webhooks. Their provenance is in
// the name: *_openapi_example files are copied from mNotify's OpenAPI file,
// *_constructed files are made up (docs/PROVIDER_VALIDATION.md).
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

// logBuffer collects log output so tests can search it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func capturedLog() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func newTestProvider(base string) *Provider {
	return New(Options{APIKey: testKey, BaseURL: base, USSDTokens: []string{testUSSDToken}, Log: quietLog()})
}

// apiCall is one request as the fake mNotify saw it.
type apiCall struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// fakeMNotify is a stand-in for api.mnotify.com. Its base URL ends in /api like
// the real one, and every request is recorded.
type fakeMNotify struct {
	*httptest.Server
	mu    sync.Mutex
	calls []apiCall
}

func newFakeMNotify(t testing.TB, api http.HandlerFunc) *fakeMNotify {
	t.Helper()
	f := &fakeMNotify{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		api(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// base is the value for MNOTIFY_BASE_URL.
func (f *fakeMNotify) base() string { return f.URL + "/api" }

func (f *fakeMNotify) recorded() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall(nil), f.calls...)
}

// respond returns a handler answering every request with status and body.
func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func outbound() channel.OutboundSMS {
	return channel.OutboundSMS{
		To: testPhone, SenderID: "SMS-AI", Body: "Photosynthesis turns light into sugar.", Encoding: "GSM-7", ClientRef: testMsgID,
	}
}

// errChain returns the text of err and everything it wraps.
func errChain(err error) string {
	var parts []string
	for ; err != nil; err = errors.Unwrap(err) {
		parts = append(parts, fmt.Sprintf("%v", err))
	}
	return strings.Join(parts, " | ")
}

// leaksKey reports whether s holds the API key in any of its URL forms.
func leaksKey(s string) bool {
	for _, form := range []string{testKey, url.QueryEscape(testKey), url.PathEscape(testKey)} {
		if strings.Contains(s, form) {
			return true
		}
	}
	return false
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
