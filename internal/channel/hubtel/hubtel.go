// Package hubtel implements Hubtel's Ghana USSD ("Programmable Services"
// Service Interaction URL), SMS send API and delivery-report callbacks (plan
// Track A: USSD ingress + alphanumeric MT SMS). docs/PROVIDER_VALIDATION.md
// records which parts of the contract Hubtel's published material confirms
// and which are still open; several are, because Hubtel's SMS API reference
// could not be read when this adapter was written.
//
// Webhook authentication: Hubtel documents no signature, shared secret or
// source-address list for USSD or delivery-report callbacks, and its own SDK
// accepts any POST. Both callbacks are therefore authenticated by a long
// random token in the registered callback URL (?token=...), optionally
// combined with a source-IP allowlist. Requests without it are refused, and
// with no token configured every request is refused.
//
// The provider does not implement channel.StatusQuerier on purpose. The send
// API accepts our ClientReference and echoes it in delivery reports, but no
// lookup by client reference was found in Hubtel's material, and a lookup by
// Hubtel's own MessageId is useless for an uncertain submission (the response
// carrying that id was lost). A "not found" answer would also make the
// reconciler resend a message that may already be delivered and billed.
package hubtel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/security"
)

const (
	Name = "hubtel"
	// DefaultBaseURL is the SMS API host named in Hubtel's Send Message
	// reference; override it with HUBTEL_SMS_BASE_URL if Hubtel gives your
	// account another one.
	DefaultBaseURL = "https://sms.hubtel.com"
	// TokenParam is the query parameter carrying the callback shared secret.
	TokenParam = "token"
	// MaxSenderIDLen is the GSM alphanumeric sender limit. Hubtel's own rule
	// is not published in the material that could be read.
	MaxSenderIDLen = 11
)

type Options struct {
	// ClientID and ClientSecret are the SMS API credentials, sent as HTTP
	// Basic authentication.
	ClientID     string
	ClientSecret string
	BaseURL      string // DefaultBaseURL when empty
	// USSDTokens and DLRTokens are the accepted values of ?token= on USSD and
	// delivery-report callbacks. More than one allows rotating the registered
	// URL without downtime. Separate lists keep a leaked delivery-report URL
	// from being usable to drive USSD sessions.
	USSDTokens []string
	DLRTokens  []string
	// USSDAllowedIPs and DLRAllowedIPs optionally restrict the callbacks to
	// these networks. Hubtel does not publish its source addresses.
	USSDAllowedIPs []netip.Prefix
	DLRAllowedIPs  []netip.Prefix
	// ClientIPHeader names a header holding the caller's address, set by a
	// reverse proxy that overwrites it. Empty uses the TCP peer address.
	ClientIPHeader string
	Log            *slog.Logger
}

type Provider struct {
	opt    Options
	base   string
	client *http.Client
	now    func() time.Time
	log    *slog.Logger
}

func New(opt Options) *Provider {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	base := strings.TrimRight(opt.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Provider{
		opt: opt, base: base, now: time.Now, log: log,
		client: &http.Client{
			// Backstop only: callers pass their own deadlines.
			Timeout: 30 * time.Second,
			// A redirected POST would be replayed as a GET, and the
			// credentials could follow it to another host; treat a redirect
			// as a misconfigured base URL instead of following it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *Provider) Name() string { return Name }

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

// flexString decodes a JSON string, number or null into a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	switch {
	case string(b) == "null":
		*f = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
	default:
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexString(n)
	}
	return nil
}
