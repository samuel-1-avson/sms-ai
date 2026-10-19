// Package mnotify implements mNotify (mnotify.com; its platform is BMS at
// bms.africa, API host https://api.mnotify.com/api), the owner's chosen
// provider for BOTH USSD and outbound SMS: the "Quick Bulk SMS" call is the MT
// SMS sender, and the "Shared USSD" and "Dedicated USSD" callbacks are the USSD
// gateway. Whether the shared USSD product works on all three Ghanaian
// networks is not published.
//
// The only authority is mNotify's public OpenAPI file,
// https://developer.bms.africa/openapi14.yaml (rendered at
// https://developer.bms.africa/). Nothing here has run against mNotify yet:
// there is no account, and docs/PROVIDER_VALIDATION.md (mNotify section) lists
// what the file confirms and what it leaves open (USSD price and short-code
// provisioning, how the callback URL is registered, callback source addresses,
// response deadline and session timeout, error codes, and more).
//
// Four properties of the API drive the design:
//
//   - The API key is a query parameter on every call (?key=...), so it would
//     appear in any URL a Go error, log line or test failure prints. The
//     client therefore never logs a request URL, strips net/url's URL echo from
//     transport errors, and scrubs the key (raw and URL-escaped) from every
//     error and response excerpt before it leaves the package.
//   - A send has no client reference or idempotency field, and the response
//     carries the only handle (the campaign id). A send whose answer was lost
//     therefore cannot be looked up, which is why Provider is NOT a
//     channel.StatusQuerier: a lookup that finds nothing would be read as "not
//     sent" and the billed message would be sent again. Uncertain outcomes wait
//     for an operator (docs/RUNBOOKS.md).
//   - mNotify documents no delivery-report webhook. ParseStatus refuses
//     everything and messages stay 'submitted'. The campaign id stored as the
//     provider message id is the key of mNotify's pull API (GET /campaign/<id>),
//     which a future poller could use; none is built.
//   - Callbacks carry no documented authentication. They are authenticated by a
//     long random token in the callback URL (?token=...), optionally combined
//     with a source-IP allowlist; with no token configured every request is
//     refused. This is the same trade-off the Hubtel and MTN adapters make; a
//     token in a URL can leak through proxy logs, which is why the pilot proxy
//     keeps no access log.
package mnotify

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"smsaighana/internal/channel"
	"smsaighana/internal/security"
)

const (
	Name = "mnotify"
	// DefaultBaseURL is the host and path prefix every operation of the OpenAPI
	// file hangs off ("https://api.mnotify.com/api/...").
	DefaultBaseURL = "https://api.mnotify.com/api"
	// USSDCallbackPath is the one route for both USSD modes; the payload shape
	// says which mode a request belongs to.
	USSDCallbackPath = "/webhooks/ussd/mnotify/session"
	// TokenParam is the query parameter carrying the callback shared secret.
	TokenParam = "token"
	// MaxSenderIDLen is mNotify's documented sender ID limit.
	MaxSenderIDLen = 11

	// USSD modes: ModeAuto detects shared or dedicated from each payload.
	ModeAuto      = "auto"
	ModeShared    = "shared"
	ModeDedicated = "dedicated"

	maxResponseBody = 64 << 10
	maxMessageIDLen = 128
)

type Options struct {
	// APIKey is the key from the mNotify dashboard (Settings > Developer). It is
	// sent as ?key= on every outbound call and must never be logged.
	APIKey string
	// BaseURL is the API host and path prefix (DefaultBaseURL when empty).
	BaseURL string
	// USSDMode is ModeAuto (also the empty value), ModeShared or ModeDedicated.
	// A fixed mode refuses callbacks of the other shape.
	USSDMode string
	// USSDTokens are the accepted values of ?token= on USSD callbacks. More than
	// one allows rotating the registered URL without downtime.
	USSDTokens []string
	// USSDAllowedIPs optionally restricts the callbacks to these networks.
	// mNotify does not publish its callback source addresses.
	USSDAllowedIPs []netip.Prefix
	// ClientIPHeader names a header holding the caller's address, set by a
	// reverse proxy that overwrites it. Empty uses the TCP peer address.
	ClientIPHeader string
	Log            *slog.Logger
}

type Provider struct {
	opt   Options
	api   *API
	log   *slog.Logger
	now   func() time.Time
	ended *endedSessions
	// lowCreditAt is when the last low-credit warning was logged (unix nanos).
	lowCreditAt atomic.Int64
}

func New(opt Options) *Provider {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	opt.USSDMode = strings.ToLower(strings.TrimSpace(opt.USSDMode))
	if opt.USSDMode == "" {
		opt.USSDMode = ModeAuto
	}
	return &Provider{opt: opt, api: newAPI(opt), log: log, now: time.Now, ended: newEndedSessions()}
}

func (p *Provider) Name() string { return Name }

// authenticate enforces the shared-secret token and, when configured, the
// source-address allowlist. With no token configured every request is refused
// rather than accepted unauthenticated.
func (p *Provider) authenticate(r *http.Request) error {
	if len(p.opt.USSDTokens) == 0 {
		return fmt.Errorf("%w: MNOTIFY_USSD_TOKEN is not configured", channel.ErrBadSignature)
	}
	got := r.URL.Query().Get(TokenParam)
	match := false
	for _, t := range p.opt.USSDTokens {
		if t != "" && security.ConstantTimeEqual(got, t) {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("%w: missing or wrong callback token", channel.ErrBadSignature)
	}
	if len(p.opt.USSDAllowedIPs) == 0 {
		return nil
	}
	if ip, ok := p.clientIP(r); ok {
		for _, prefix := range p.opt.USSDAllowedIPs {
			if prefix.Contains(ip) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: source address is not in the allowlist", channel.ErrBadSignature)
}

// clientIP returns the caller's address: the right-most entry of the
// configured proxy header (the hop our own proxy appended), else the peer.
func (p *Provider) clientIP(r *http.Request) (netip.Addr, bool) {
	raw := r.RemoteAddr
	if h := p.opt.ClientIPHeader; h != "" {
		v := r.Header.Get(h)
		if i := strings.LastIndexByte(v, ','); i >= 0 {
			v = v[i+1:]
		}
		raw = strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	ip, err := netip.ParseAddr(strings.Trim(raw, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// flexString decodes a JSON string or number into a string; null, booleans,
// objects and arrays become "". mNotify's examples mix types (an id printed as
// a number in one report and a string in another), and a field of another type
// must not make a whole response or callback unreadable.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = ""
	if len(b) == 0 {
		return nil
	}
	switch c := b[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
	case c == '-' || (c >= '0' && c <= '9'):
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexString(n)
	}
	return nil
}

// flexNum decodes a JSON number or a numeric string; anything else leaves it
// unset.
type flexNum struct {
	N   int64
	Set bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	*f = flexNum{}
	var s flexString
	if err := s.UnmarshalJSON(b); err != nil {
		return nil
	}
	n := json.Number(strings.TrimSpace(string(s)))
	if i, err := n.Int64(); err == nil {
		*f = flexNum{N: i, Set: true}
	} else if fl, err := n.Float64(); err == nil {
		*f = flexNum{N: int64(fl), Set: true}
	}
	return nil
}

func truncate(s string) string {
	if len(s) <= 200 {
		return s
	}
	return clip(s, 200) + "..."
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
