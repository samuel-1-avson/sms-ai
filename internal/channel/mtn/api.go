package mtn

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// API paths, from the servers/basePath entries of MTN's Swagger files joined
// with their operation paths. The USSD and SMS V2 products both sit under /v1.
const (
	smsSendPath         = "/v1/messages/sms/outbound"
	ussdSubscriptionAPI = "/v1/messages/ussd/subscription"
)

// smsSubscriptionPath is the SMS V2 delivery-notification subscription of a
// sender address: POST creates it, DELETE .../{id} removes it.
func smsSubscriptionPath(sender string) string {
	return smsSendPath + "/" + url.PathEscape(sender) + "/subscription"
}

// smsStatusPath is the SMS V2 delivery-status lookup of one request.
func smsStatusPath(sender, requestID string) string {
	return smsSendPath + "/" + url.PathEscape(sender) + "/" + url.PathEscape(requestID) + "/deliveryStatus"
}

// API is the authenticated HTTP plumbing shared by the SMS sender and the
// subscription client.
type API struct {
	base   string
	apiKey string
	tokens *TokenSource
	client *http.Client
}

// authKind is a set of credentials to attach to a call.
type authKind int

const (
	// authBearer is the OAuth access token of the SMS V2 API.
	authBearer authKind = 1 << iota
	// authAPIKey is the x-api-key header of the USSD API.
	authAPIKey
)

func newHTTPClient() *http.Client {
	return &http.Client{
		// Backstop only: callers pass their own deadlines.
		Timeout: 30 * time.Second,
		// A redirected POST would be replayed as a GET, and the credentials
		// could follow it to another host; treat a redirect as a misconfigured
		// base URL instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newAPI(opt Options) *API {
	base := strings.TrimRight(opt.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	tokenURL := opt.TokenURL
	if tokenURL == "" {
		tokenURL = base + DefaultTokenPath
	}
	key := opt.APIKey
	if key == "" {
		key = opt.ClientID
	}
	client := newHTTPClient()
	return &API{
		base: base, apiKey: key, client: client,
		tokens: NewTokenSource(tokenURL, opt.ClientID, opt.ClientSecret, client),
	}
}

// smsAuth is what SMS V2 calls carry: the bearer token, plus the API key when
// the operator found MTN wants it.
func smsAuth(opt Options) authKind {
	if opt.SMSSendsAPIKey {
		return authBearer | authAPIKey
	}
	return authBearer
}

// ussdAuth is what USSD registration calls carry: the API key, plus the bearer
// token when the operator found MTN wants it.
func ussdAuth(opt Options) authKind {
	if opt.USSDSendsBearer {
		return authAPIKey | authBearer
	}
	return authAPIKey
}

// do performs an authenticated call. written reports whether the request was
// completely sent before any transport error, which decides between retryable
// and uncertain failures; status is 0 without a response. A failure to obtain
// the OAuth token returns before anything is sent, so written is false.
func (a *API) do(req *http.Request, auth authKind) (status int, body []byte, written bool, err error) {
	if err := refuseInTests(req.URL.String()); err != nil {
		return 0, nil, false, err
	}
	var bearer string
	if auth&authBearer != 0 {
		bearer, err = a.tokens.Token(req.Context())
		if err != nil {
			return 0, nil, false, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if auth&authAPIKey != 0 {
		req.Header.Set("x-api-key", a.apiKey)
	}
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, wrote.Load(), errorf("request failed: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode == http.StatusUnauthorized && bearer != "" {
		// The token was refused (revoked, or expired earlier than advertised):
		// make the next call fetch a fresh one.
		a.tokens.Invalidate(bearer)
	}
	return resp.StatusCode, body, true, err
}

func (a *API) url(path string) string { return a.base + path }

func errorf(format string, args ...any) error { return fmt.Errorf("mtn: "+format, args...) }
