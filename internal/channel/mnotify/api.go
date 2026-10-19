package mnotify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Paths of the operations used, relative to the base URL (the OpenAPI file's
// paths under https://api.mnotify.com/api).
const (
	smsQuickPath    = "/sms/quick"
	balanceSMSPath  = "/balance/sms"
	senderStatusAPI = "/senderid/status"
)

// API is the HTTP plumbing shared by the SMS sender and the admin commands. It
// is the only code that talks to mNotify, and the only place the API key is
// put into a URL.
type API struct {
	base   string
	key    string
	client *http.Client
}

func newHTTPClient() *http.Client {
	return &http.Client{
		// Backstop only: callers pass their own deadlines.
		Timeout: 30 * time.Second,
		// A redirected POST would be replayed as a GET, and the key (which is in
		// the URL) could follow it to another host; treat a redirect as a
		// misconfigured base URL instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newAPI(opt Options) *API {
	base := strings.TrimRight(opt.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &API{base: base, key: opt.APIKey, client: newHTTPClient()}
}

// scrub removes the API key, as it appears raw and in its URL-escaped forms,
// from s. It is applied to every error message and every excerpt of a response
// body, whatever their source: Go's url.Error repeats the full request URL,
// and a server may echo what it was sent.
func (a *API) scrub(s string) string {
	if a.key == "" {
		return s
	}
	for _, form := range []string{a.key, url.QueryEscape(a.key), url.PathEscape(a.key)} {
		if form != "" {
			s = strings.ReplaceAll(s, form, "REDACTED")
		}
	}
	return s
}

// fail builds a transport or request error without the URL that net/http puts
// in *url.Error, and with the key scrubbed from whatever remains. It
// deliberately does not wrap err: the wrapped chain would still print the URL.
func (a *API) fail(what string, err error) error {
	return fmt.Errorf("mnotify: %s: %s", what, a.scrub(unwrapURLError(err).Error()))
}

// unwrapURLError returns the cause inside a *url.Error, dropping the method
// and URL (which hold the API key) that its Error() text repeats.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// do performs a call: the API key is added to the query string next to the
// extra parameters. written reports whether the request was completely sent
// before any transport error, which decides between retryable and uncertain
// failures; status is 0 without a response. The request URL is never logged
// and never appears in a returned error.
func (a *API) do(ctx context.Context, method, path string, query url.Values, body []byte) (status int, raw []byte, written bool, err error) {
	if err := refuseInTests(a.base); err != nil {
		return 0, nil, false, err
	}
	if a.key == "" {
		return 0, nil, false, errors.New("mnotify: MNOTIFY_API_KEY is not set")
	}
	q := url.Values{}
	for k, vs := range query {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("key", a.key)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path+"?"+q.Encode(), rd)
	if err != nil {
		return 0, nil, false, a.fail("building the request", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, wrote.Load(), a.fail("request failed", err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		err = a.fail("reading the response", err)
	}
	return resp.StatusCode, raw, true, err
}
