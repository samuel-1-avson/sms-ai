package mtn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultTokenLifetime applies when the token response carries no usable
	// expires_in. MTN's example shows "3599".
	defaultTokenLifetime = 5 * time.Minute
	// tokenSkew is how long before expiry a cached token is replaced, so a
	// request in flight does not carry a token that expires on the way.
	tokenSkew = 60 * time.Second
)

// TokenSource obtains and caches the OAuth 2.0 client-credentials access
// token that MTN's SMS API requires (MTN: "Understanding OAuth 2.0"). It is
// safe for concurrent use: callers that find the cache empty or stale share
// one token request instead of each sending their own.
type TokenSource struct {
	tokenURL     string
	clientID     string
	clientSecret string
	client       *http.Client
	now          func() time.Time

	mu       sync.Mutex
	token    string
	expires  time.Time
	inflight *tokenFetch
}

type tokenFetch struct {
	done    chan struct{}
	token   string
	expires time.Time
	err     error
}

// NewTokenSource returns a source for tokenURL. A nil client gets one that
// does not follow redirects, so the client secret cannot be replayed to
// another host.
func NewTokenSource(tokenURL, clientID, clientSecret string, client *http.Client) *TokenSource {
	if client == nil {
		client = newHTTPClient()
	}
	return &TokenSource{tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret, client: client, now: time.Now}
}

// Token returns a valid access token, requesting a new one when the cached one
// is missing or about to expire.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	for {
		s.mu.Lock()
		if s.token != "" && s.now().Before(s.expires) {
			t := s.token
			s.mu.Unlock()
			return t, nil
		}
		f := s.inflight
		leader := f == nil
		if leader {
			f = &tokenFetch{done: make(chan struct{})}
			s.inflight = f
		}
		s.mu.Unlock()

		if leader {
			f.token, f.expires, f.err = s.fetch(ctx)
			s.mu.Lock()
			if f.err == nil {
				s.token, s.expires = f.token, f.expires
			}
			s.inflight = nil
			s.mu.Unlock()
			close(f.done)
			return f.token, f.err
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if f.err == nil {
			return f.token, nil
		}
		// The leader's own context ending says nothing about ours; go round
		// again and, if nobody else has, fetch with our context.
		if (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) && ctx.Err() == nil {
			continue
		}
		return "", f.err
	}
}

// Invalidate drops the cached token if it is still the given one, after MTN
// answered a request made with it with 401.
func (s *TokenSource) Invalidate(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == token {
		s.token, s.expires = "", time.Time{}
	}
}

// tokenResponse is the part of the token endpoint's reply that is used. MTN's
// example carries expires_in as a string ("3599"); a number is accepted too.
type tokenResponse struct {
	AccessToken string     `json:"access_token"`
	ExpiresIn   flexString `json:"expires_in"`
}

func (s *TokenSource) fetch(ctx context.Context) (string, time.Time, error) {
	// MTN's example: POST <token URL>?grant_type=client_credentials with the
	// consumer key and secret as a form body.
	u, err := url.Parse(s.tokenURL)
	if err != nil {
		return "", time.Time{}, errors.New("mtn: token URL is not a valid URL")
	}
	q := u.Query()
	if q.Get("grant_type") == "" {
		q.Set("grant_type", "client_credentials")
	}
	u.RawQuery = q.Encode()
	form := url.Values{"client_id": {s.clientID}, "client_secret": {s.clientSecret}}

	if err := refuseInTests(u.String()); err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mtn: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		// A *url.Error would repeat the URL; it holds no secret (the secret is
		// in the body) but keep the message short.
		return "", time.Time{}, fmt.Errorf("mtn: token request failed: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mtn: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("mtn: token request answered HTTP %d: %s", resp.StatusCode, detail(raw))
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return "", time.Time{}, errors.New("mtn: token response has no access_token")
	}
	life := defaultTokenLifetime
	if secs, err := strconv.ParseFloat(string(tr.ExpiresIn), 64); err == nil && secs > 0 {
		life = time.Duration(secs * float64(time.Second))
	}
	skew := tokenSkew
	if life < 4*skew {
		skew = life / 4
	}
	return tr.AccessToken, s.now().Add(life - skew), nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
