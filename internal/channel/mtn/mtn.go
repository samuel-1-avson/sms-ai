// Package mtn implements MTN's own developer platform (developers.mtn.com,
// API host api.mtn.com) for Ghana: the "MTN USSD interface" product as the USSD
// gateway, the "SMS V2" product (MTN's "SMS V2 interface with callback
// support") as the MT SMS sender, and its delivery-notification callback. The
// bearer tokens for SMS come from the "OAuth V1" product. MTN's network only:
// Telecel and AT subscribers cannot dial an MTN USSD code or be reached through
// MTN's SMS API.
//
// Everything here is built from MTN's published Swagger files; nothing has run
// against MTN yet because there are no credentials. docs/PROVIDER_VALIDATION.md
// lists what the files confirm and what is still open (pricing, onboarding,
// short-code allocation, callback source addresses, the delivery-notification
// body, which credential each API really wants, the MSISDN format Ghana really
// sends, and more). Validate in MTN's sandbox before real traffic.
//
// SMS V2 vs the separate "SMS v3 API" product: the owner's app uses V2. V2 is
// the one with a delivery-status lookup, but it publishes no delivery-
// notification body (v3 does), so the parser accepts the plausible shapes and
// is the least certain part of the SMS half.
//
// Webhook authentication: MTN documents no signature or shared secret for
// callbacks, and its developer notes say the platform does not support
// authentication on the client's callback endpoint at all. Both callbacks are
// therefore authenticated by a long random token in the registered callback
// URL (?token=...), optionally combined with a source-IP allowlist. Requests
// without it are refused, and with no token configured every request is
// refused. This is the same trade-off the Hubtel adapter makes; a token in a
// URL can leak through proxy logs, which is why the pilot proxy keeps no
// access log.
//
// Reconciliation: V2 can look a message up by the requestId it returned from
// the send call, which a submission whose response was lost does not have. The
// provider therefore implements channel.StatusQuerier only when
// Options.LookupByClientRef is set (see lookup.go), because whether MTN accepts
// our own clientCorrelator in that path is not documented. Even then a lookup
// that does not find the message is reported as an error, never as "not
// found": a false "not found" would make the reconciler resend a message that
// may already be delivered and billed.
package mtn

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"smsaighana/internal/channel"
	"smsaighana/internal/security"
)

const (
	Name = "mtn"
	// DefaultBaseURL is the host in the "host" and "servers" entries of MTN's
	// Swagger files. The USSD and SMS V2 APIs both sit under /v1/ on it. MTN
	// also names {opco}.api.mtn.com hosts and sandbox.api.mtn.com; override
	// MTN_BASE_URL to use one.
	DefaultBaseURL = "https://api.mtn.com"
	// DefaultTokenPath is appended to the base URL for the OAuth token
	// endpoint: "OAuth V1" (basePath /v1/oauth, POST /access_token), the SMS V2
	// Swagger's tokenUrl and MTN's "Understanding OAuth 2.0" page agree on it.
	// (The separate SMS v3 Swagger names ".../access_token/accesstoken".)
	DefaultTokenPath = "/v1/oauth/access_token"
	// DefaultCountryCode is the value of the x-country-code header that the SMS
	// V2 subscription calls require. MTN does not say which format it wants;
	// it says it uses ISO 3166 codes, and "GH" is the alpha-2 one.
	DefaultCountryCode = "GH"
	// TokenParam is the query parameter carrying the callback shared secret.
	TokenParam = "token"
	// MaxSenderIDLen is the GSM alphanumeric sender limit. MTN's sender rules
	// are not published.
	MaxSenderIDLen = 11
	// MaxClientRefLen bounds the clientCorrelator we send. The SMS V2 Swagger
	// gives no limit; the (separate) v3 one says 36, and our message ids are
	// UUIDs of exactly that length.
	MaxClientRefLen = 36

	maxResponseBody = 64 << 10
	maxMessageIDLen = 128
)

type Options struct {
	// ClientID and ClientSecret are the app's Consumer Key and Secret from the
	// developer portal, used for the OAuth V1 client-credentials token that the
	// SMS V2 API needs.
	ClientID     string
	ClientSecret string
	// APIKey is the x-api-key header of the USSD API. MTN's FAQ says the key
	// is the app's Consumer Key, so it defaults to ClientID.
	APIKey string
	// BaseURL is the API host (DefaultBaseURL when empty); TokenURL defaults to
	// BaseURL + DefaultTokenPath.
	BaseURL  string
	TokenURL string
	// USSDServiceCode is echoed in USSD replies (data.serviceCode).
	USSDServiceCode string
	// SenderID is the senderAddress of SMS V2 sends (also the path parameter of
	// the delivery subscription and the delivery-status lookup). Send takes the
	// sender from each message; this is for the calls that have no message.
	SenderID string
	// CountryCode is the x-country-code header of the SMS subscription calls
	// (DefaultCountryCode when empty).
	CountryCode string
	// The two flags below exist because MTN's files disagree about which
	// credential each API wants (docs/PROVIDER_VALIDATION.md). SMSSendsAPIKey
	// adds the x-api-key header to SMS V2 calls, which the Swagger declares
	// OAuth-only; USSDSendsBearer adds the OAuth bearer token to the USSD
	// registration calls, which the Swagger declares API-key-only.
	SMSSendsAPIKey  bool
	USSDSendsBearer bool
	// LookupByClientRef makes the provider a channel.StatusQuerier that tries
	// our own client reference as the requestId of the delivery-status lookup.
	// Off by default: whether MTN accepts it is not documented.
	LookupByClientRef bool
	// USSDInputIsHistory is for the case where MTN's ussdString turns out to
	// carry the whole "1*2*text" history instead of the latest entry: only the
	// part after the last "*" is then used. MTN does not say which it sends.
	USSDInputIsHistory bool
	// USSDTokens and DLRTokens are the accepted values of ?token= on USSD and
	// delivery-notification callbacks. More than one allows rotating the
	// registered URL without downtime. Separate lists keep a leaked
	// delivery-notification URL from being usable to drive USSD sessions.
	USSDTokens []string
	DLRTokens  []string
	// USSDAllowedIPs and DLRAllowedIPs optionally restrict the callbacks to
	// these networks. MTN does not publish its callback source addresses.
	USSDAllowedIPs []netip.Prefix
	DLRAllowedIPs  []netip.Prefix
	// ClientIPHeader names a header holding the caller's address, set by a
	// reverse proxy that overwrites it. Empty uses the TCP peer address.
	ClientIPHeader string
	Log            *slog.Logger
}

type Provider struct {
	opt    Options
	api    *API
	tokens *TokenSource
	now    func() time.Time
	log    *slog.Logger
}

func New(opt Options) *Provider {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	api := newAPI(opt)
	return &Provider{opt: opt, api: api, tokens: api.tokens, now: time.Now, log: log}
}

func (p *Provider) Name() string { return Name }

// Tokens returns the OAuth token source, for the credential check of the
// admin tool.
func (p *Provider) Tokens() *TokenSource { return p.tokens }

// authenticate enforces the shared-secret token and, when configured, the
// source-address allowlist. With no token configured every request is
// refused rather than accepted unauthenticated.
func (p *Provider) authenticate(r *http.Request, tokens []string, allowed []netip.Prefix, tokenEnv string) error {
	if len(tokens) == 0 {
		return fmt.Errorf("%w: %s is not configured", channel.ErrBadSignature, tokenEnv)
	}
	got := r.URL.Query().Get(TokenParam)
	match := false
	for _, t := range tokens {
		if t != "" && security.ConstantTimeEqual(got, t) {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("%w: missing or wrong callback token", channel.ErrBadSignature)
	}
	if len(allowed) == 0 {
		return nil
	}
	if ip, ok := p.clientIP(r); ok {
		for _, prefix := range allowed {
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

// RedactURL hides the callback token in a registered URL, for output that
// may end up in a terminal log or a ticket.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	q := u.Query()
	if q.Has(TokenParam) {
		q.Set(TokenParam, "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// flexString decodes a JSON string or number into a string; null, booleans,
// objects and arrays become "". MTN's Swagger types messageType as a string on
// requests and an integer on replies, and a field of another type must not
// make a whole callback unreadable.
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

func sendErr(kind channel.SendErrorKind, format string, args ...any) error {
	return &channel.SendError{Kind: kind, Err: fmt.Errorf("mtn: "+format, args...)}
}

// detail extracts MTN's error text for logs and send_attempts. It never echoes
// the request, so no phone number, message text or credential leaks. MTN's
// error bodies carry statusCode (a four-digit canonical code), statusMessage
// and supportMessage.
func detail(raw []byte) string {
	var e struct {
		StatusCode     flexString `json:"statusCode"`
		StatusMessage  string     `json:"statusMessage"`
		SupportMessage string     `json:"supportMessage"`
		Error          string     `json:"error"`
		Message        string     `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return "no error message"
	}
	var parts []string
	if e.StatusCode != "" {
		parts = append(parts, "code "+string(e.StatusCode))
	}
	for _, s := range []string{e.StatusMessage, e.SupportMessage, e.Error, e.Message} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
			break
		}
	}
	if len(parts) == 0 {
		return "no error message"
	}
	return truncate(strings.Join(parts, ": "))
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
