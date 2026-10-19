package mnotify

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/config"
)

// TestNoErrorOrLogContainsTheAPIKey runs every way a call can fail and checks
// that the API key, which is in the query string of each request, appears in
// no returned error, no wrapped error and no log line, in either its raw or its
// URL-escaped form. Go's *url.Error repeats the full request URL, and servers
// may echo what they are sent, so each path matters.
func TestNoErrorOrLogContainsTheAPIKey(t *testing.T) {
	echo := func(status int) http.HandlerFunc {
		// A hostile or careless server: repeats the whole request URI, key
		// included, raw and escaped, in its error message.
		return func(w http.ResponseWriter, r *http.Request) {
			msg := fmt.Sprintf("bad request %s (key %s)", r.URL.RequestURI(), testKey)
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"status":"error","code":"4000","message":%q}`, msg)
		}
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedBase := closed.URL + "/api"
	closed.Close()

	for _, c := range []struct {
		name string
		base func(t *testing.T) string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"connection refused", func(*testing.T) string { return closedBase }, nil},
		{"timeout while waiting", func(t *testing.T) string {
			return newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }).base()
		}, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}},
		{"dropped connection", func(t *testing.T) string {
			return newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
			}).base()
		}, nil},
		{"server echoes the key in a 200", func(t *testing.T) string { return newFakeMNotify(t, echo(200)).base() }, nil},
		{"server echoes the key in a 400", func(t *testing.T) string { return newFakeMNotify(t, echo(400)).base() }, nil},
		{"server echoes the key in a 401", func(t *testing.T) string { return newFakeMNotify(t, echo(401)).base() }, nil},
		{"server echoes the key in a 500", func(t *testing.T) string { return newFakeMNotify(t, echo(500)).base() }, nil},
		{"server echoes the key in a 302 body", func(t *testing.T) string {
			return newFakeMNotify(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/?"+r.URL.RawQuery)
				w.WriteHeader(302)
				fmt.Fprintf(w, `{"message":"moved to %s"}`, r.URL.RawQuery)
			}).base()
		}, nil},
		{"unparseable base URL", func(*testing.T) string { return "http://127.0.0.1:1/api\x7f" }, nil},
		{"base URL without a host", func(*testing.T) string { return "/api" }, nil},
		{"base URL with an unsupported scheme", func(*testing.T) string { return "gopher://127.0.0.1:1/api" }, nil},
		{"test guard on the real host", func(*testing.T) string { return DefaultBaseURL }, nil},
		{"test guard on a subdomain", func(*testing.T) string { return "https://eu.api.mnotify.com/api" }, nil},
		{"test guard on the BMS host", func(*testing.T) string { return "https://api.bms.africa/api" }, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			log, logs := capturedLog()
			p := New(Options{APIKey: testKey, BaseURL: c.base(t), Log: log})
			ctx := context.Background()
			if c.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = c.ctx()
				defer cancel()
			}
			var texts []string
			_, err := p.Send(ctx, outbound())
			if err == nil {
				t.Fatal("the send should have failed")
			}
			texts = append(texts, fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), errChain(err))
			if se, ok := err.(*channel.SendError); ok {
				texts = append(texts, se.Err.Error())
			}

			// The admin commands go through the same plumbing.
			cfg := adminConfig(t, p.api.base)
			for _, args := range [][]string{{"mnotify-status", "-check-credentials"}, {"mnotify-sender-status"}} {
				var out bytes.Buffer
				err := RunAdmin(ctx, args[0], args[1:], cfg, &out)
				texts = append(texts, out.String(), errString(err), errChain(err))
			}
			texts = append(texts, logs.String())

			for _, s := range texts {
				if leaksKey(s) {
					t.Fatalf("the API key leaked: %q", s)
				}
			}
		})
	}
}

// TestKeyNeverAppearsInLogsOfNormalOperation: successful and failed sends log
// nothing that contains the key, a request URL, or a phone number.
func TestKeyNeverAppearsInLogsOfNormalOperation(t *testing.T) {
	log, logs := capturedLog()
	fake := newFakeMNotify(t, respond(200, `{"status":"success","code":"2000","summary":{"_id":"C","total_sent":1,"credit_left":3}}`))
	p := New(Options{APIKey: testKey, BaseURL: fake.base(), USSDTokens: []string{testUSSDToken}, Log: log})
	if _, err := p.Send(context.Background(), outbound()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ParseSession(ussdHTTP("wrong"), []byte(`{}`)); err == nil {
		t.Fatal("want a refusal")
	}
	out := logs.String()
	if out == "" {
		t.Fatal("expected the low-credit warning to be logged")
	}
	for _, bad := range []string{testKey, "key=", "/api/sms/quick", "http://", "0541234567", "233541234567", testUSSDToken} {
		if strings.Contains(out, bad) {
			t.Fatalf("log output contains %q:\n%s", bad, out)
		}
	}
}

func TestScrub(t *testing.T) {
	a := &API{key: testKey}
	in := "raw " + testKey + " escaped " + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(testKey, "+", "%2B"), "/", "%2F"), "=", "%3D") + " end"
	if got := a.scrub(in); leaksKey(got) || !strings.Contains(got, "REDACTED") || !strings.HasSuffix(got, " end") {
		t.Fatalf("scrub: %q", got)
	}
	// With no key configured the text is unchanged (not a REDACTED between every character).
	if got := (&API{}).scrub("plain text"); got != "plain text" {
		t.Fatalf("scrub without a key: %q", got)
	}
}

func TestTestGuardRefusesMNotifyHosts(t *testing.T) {
	for _, u := range []string{
		"https://api.mnotify.com/api", "https://mnotify.com", "https://API.MNOTIFY.COM/api", "http://eu.api.mnotify.com:8443/x",
		"https://api.bms.africa/api", "https://bms.africa", "https://developer.bms.africa/openapi14.yaml",
	} {
		if err := refuseInTests(u); err == nil {
			t.Errorf("%s was not refused", u)
		} else if leaksKey(err.Error()) || strings.Contains(err.Error(), "?") {
			t.Errorf("guard error echoes the URL: %v", err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8080/api", "http://localhost/api", "https://notmnotify.com/api", "https://mnotify.com.example.org/api", "https://api.example.org/bms.africa", "%"} {
		if err := refuseInTests(u); err != nil {
			t.Errorf("%s was refused: %v", u, err)
		}
	}
}

func adminConfig(t testing.TB, base string, extra ...string) *config.Config {
	t.Helper()
	env := append([]string{
		"APP_ENV=test", "PRIMARY_SMS_PROVIDER=mnotify", "PRIMARY_USSD_PROVIDER=mnotify", "SMS_ENVIRONMENT=live",
		"MNOTIFY_API_KEY=" + testKey, "MNOTIFY_BASE_URL=" + base, "MNOTIFY_USSD_TOKEN=" + testUSSDToken,
	}, extra...)
	cfg, err := config.LoadFrom(env)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
