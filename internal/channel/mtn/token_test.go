package mtn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable time source for the token cache.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func newTokenSource(base string, c *clock) *TokenSource {
	ts := NewTokenSource(base+DefaultTokenPath, testClientID, testSecret, nil)
	ts.now = c.now
	return ts
}

func TestTokenRequestFormatAndDocExample(t *testing.T) {
	var gotQuery url.Values
	var gotForm url.Values
	var gotMethod, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotQuery, gotMethod, gotType = r.URL.Query(), r.Method, r.Header.Get("Content-Type")
		gotForm, _ = url.ParseQuery(string(body))
		// MTN's documented response, copied from its OAuth page.
		_, _ = w.Write(fixture(t, "mtn_oauth_token_response_doc_example.json"))
	}))
	defer srv.Close()
	ts := newTokenSource(srv.URL, newClock())
	tok, err := ts.Token(context.Background())
	if err != nil || tok != "GTPY9VUHCqKVMRB0cHxnmAp0utR0" {
		t.Fatalf("token %q: %v", tok, err)
	}
	// "POST <token URL>?grant_type=client_credentials" with the consumer key
	// and secret as a form body.
	if gotMethod != http.MethodPost || gotQuery.Get("grant_type") != "client_credentials" ||
		!strings.HasPrefix(gotType, "application/x-www-form-urlencoded") ||
		gotForm.Get("client_id") != testClientID || gotForm.Get("client_secret") != testSecret {
		t.Fatalf("request: %s query %v form %v type %q", gotMethod, gotQuery, gotForm, gotType)
	}
	if strings.Contains(gotQuery.Encode(), testSecret) {
		t.Fatal("the client secret must travel in the body, not the URL")
	}
	// An explicit token URL that already carries grant_type
	// is not given a second one.
	ts2 := NewTokenSource(srv.URL+DefaultTokenPath+"?grant_type=client_credentials", testClientID, testSecret, nil)
	if _, err := ts2.Token(context.Background()); err != nil || len(gotQuery["grant_type"]) != 1 {
		t.Fatalf("grant_type duplicated: %v %v", gotQuery, err)
	}
}

func TestTokenIsCachedAndRefreshedBeforeExpiry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":"3599"}`, n)
	}))
	defer srv.Close()
	c := newClock()
	ts := newTokenSource(srv.URL, c)
	get := func() string {
		t.Helper()
		tok, err := ts.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if get() != "tok-1" || get() != "tok-1" || calls.Load() != 1 {
		t.Fatalf("a valid token must be reused (calls=%d)", calls.Load())
	}
	c.advance(3499 * time.Second) // 3599 s lifetime minus the 60 s margin is 3539 s
	if get() != "tok-1" {
		t.Fatal("token replaced too early")
	}
	c.advance(41 * time.Second) // 3540 s: inside the margin
	if get() != "tok-2" || calls.Load() != 2 {
		t.Fatalf("token should have been refreshed (calls=%d)", calls.Load())
	}
	if get() != "tok-2" || calls.Load() != 2 {
		t.Fatalf("refreshed token must be cached (calls=%d)", calls.Load())
	}
	// A rejected token is dropped; dropping a stale one is a no-op.
	ts.Invalidate("tok-1")
	if get() != "tok-2" {
		t.Fatal("Invalidate of an old token dropped the current one")
	}
	ts.Invalidate("tok-2")
	if get() != "tok-3" || calls.Load() != 3 {
		t.Fatalf("invalidated token should be replaced (calls=%d)", calls.Load())
	}
}

func TestTokenOAuthV1SwaggerExample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "mtn_oauth_token_response_swagger_example.json"))
	}))
	defer srv.Close()
	ts := newTokenSource(srv.URL, newClock())
	if tok, err := ts.Token(context.Background()); err != nil || tok != "Q3C3dhzGxB4I8AZJ6ldPxuSB3Y6o" {
		t.Fatalf("OAuth V1 swagger example: token=%q err=%v", tok, err)
	}
}

func TestTokenLifetimes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		life time.Duration // how long until the cached token is replaced
	}{
		"string expires_in":  {`{"access_token":"a","expires_in":"3599"}`, 3599*time.Second - tokenSkew},
		"numeric expires_in": {`{"access_token":"a","expires_in":120}`, 120*time.Second - 30*time.Second},
		"short lifetime":     {`{"access_token":"a","expires_in":"40"}`, 30 * time.Second},
		"missing expires_in": {`{"access_token":"a"}`, defaultTokenLifetime - tokenSkew},
		"zero expires_in":    {`{"access_token":"a","expires_in":"0"}`, defaultTokenLifetime - tokenSkew},
		"garbage expires_in": {`{"access_token":"a","expires_in":"soon"}`, defaultTokenLifetime - tokenSkew},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.WriteString(w, tc.body)
		}))
		c := newClock()
		ts := newTokenSource(srv.URL, c)
		_, _ = ts.Token(context.Background())
		c.advance(tc.life - time.Second)
		_, _ = ts.Token(context.Background())
		if calls.Load() != 1 {
			t.Errorf("%s: replaced before %v", name, tc.life)
		}
		c.advance(2 * time.Second)
		_, _ = ts.Token(context.Background())
		if calls.Load() != 2 {
			t.Errorf("%s: not replaced after %v", name, tc.life)
		}
		srv.Close()
	}
}

func TestTokenConcurrentCallersShareOneRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		time.Sleep(150 * time.Millisecond) // everyone arrives while this is in flight
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":"3599"}`, n)
	}))
	defer srv.Close()
	ts := newTokenSource(srv.URL, newClock())

	const callers = 50
	got := make([]string, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := ts.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			got[i] = tok
		}()
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d concurrent callers sent %d token requests, want 1", callers, calls.Load())
	}
	for i, tok := range got {
		if tok != "tok-1" {
			t.Fatalf("caller %d got %q", i, tok)
		}
	}
}

func TestTokenConcurrentUseWithRefreshAndInvalidate(t *testing.T) {
	// Run with -race: readers, refreshes and invalidations interleave.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":"61"}`, n) // always inside the margin after 1 s
	}))
	defer srv.Close()
	c := newClock()
	ts := newTokenSource(srv.URL, c)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				tok, err := ts.Token(context.Background())
				if err != nil || !strings.HasPrefix(tok, "tok-") {
					t.Errorf("token %q: %v", tok, err)
					return
				}
				switch j % 5 {
				case 2:
					c.advance(2 * time.Second)
				case 4:
					ts.Invalidate(tok)
				}
			}
		}()
	}
	wg.Wait()
}

func TestTokenLeaderCancellationDoesNotFailOtherCallers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":"3599"}`, n)
	}))
	defer srv.Close()
	ts := newTokenSource(srv.URL, newClock())

	leaderCtx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() { _, err := ts.Token(leaderCtx); leaderErr <- err }()
	time.Sleep(20 * time.Millisecond) // the leader's request is in flight

	tok, err := ts.Token(context.Background())
	if err != nil || !strings.HasPrefix(tok, "tok-") {
		t.Fatalf("follower got %q: %v", tok, err)
	}
	if err := <-leaderErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("leader error %v", err)
	}
}

func TestTokenFollowerHonoursItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, `{"access_token":"a","expires_in":"3599"}`)
	}))
	defer srv.Close()
	defer close(release)
	ts := newTokenSource(srv.URL, newClock())
	go func() { _, _ = ts.Token(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ts.Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiting caller must give up with its own context, got %v", err)
	}
}

func TestTokenErrorsNeverContainCredentials(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"OAuth V1 invalid client": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		},
		"401 echoing the request": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"statusCode":"4000","statusMessage":"Unauthorised","supportMessage":"Invalid client"}`)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example.net/steal", http.StatusFound)
		},
		"bad JSON": func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "not json") },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		_, err := newTokenSource(srv.URL, newClock()).Token(context.Background())
		srv.Close()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), testClientID) {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
	}
	// The redirect is not followed, so the secret cannot reach another host.
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := newTokenSource(srv.URL, newClock()).Token(context.Background()); err == nil || elsewhere.Load() != 0 {
		t.Fatalf("redirect followed (%d hits) or accepted: %v", elsewhere.Load(), err)
	}
}
