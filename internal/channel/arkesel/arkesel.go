// Package arkesel implements Arkesel's Ghana USSD session callbacks, SMS v2
// sending and signed delivery reports (plan Track A: USSD ingress +
// alphanumeric MT SMS). docs/PROVIDER_VALIDATION.md records which parts of
// the contract are confirmed by Arkesel's documentation and which are not.
//
// Webhook authentication:
//   - Delivery reports: X-Arkesel-Webhook-Signature = "v1=" + hex(HMAC-SHA256(
//     secret, timestamp + "." + canonical JSON of the query parameters)), with
//     two comma-separated values while a secret rotation is in progress.
//   - USSD: Arkesel does not sign USSD callbacks, so the endpoint URL
//     registered with Arkesel carries a shared secret (?token=...), optionally
//     combined with a source-IP allowlist. Requests without it are refused.
//
// The provider does not implement channel.StatusQuerier on purpose. Arkesel
// can only look a message up by its own UUID, which an uncertain submission
// (the response was lost) never has, and a "not found" answer would make the
// reconciler resend a message that may already be delivered and billed.
// Lookup is available for manual checks instead.
package arkesel

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

	"smsaighana/internal/channel"
	"smsaighana/internal/security"
)

const (
	Name            = "arkesel"
	DefaultBaseURL  = "https://sms.arkesel.com"
	HeaderSignature = "X-Arkesel-Webhook-Signature"
	HeaderTimestamp = "X-Arkesel-Webhook-Timestamp"
	HeaderWebhookID = "X-Arkesel-Webhook-Id"
	// TokenParam is the query parameter carrying the USSD shared secret.
	TokenParam = "token"
	// MaxSenderIDLen is Arkesel's documented limit, spaces included.
	MaxSenderIDLen = 11
)

type Options struct {
	APIKey  string
	BaseURL string // DefaultBaseURL when empty
	// CallbackURL is sent as callback_url with every message so Arkesel
	// posts delivery reports; empty disables receipts.
	CallbackURL string
	// Sandbox sends with sandbox=true: accepted, not billed, never delivered.
	Sandbox bool
	// WebhookSecret verifies delivery reports (GET /api/v2/webhooks/secret).
	WebhookSecret string
	// USSDTokens are accepted values of ?token= on USSD callbacks; more than
	// one allows rotating the URL registered with Arkesel.
	USSDTokens []string
	// AllowedIPs optionally restricts USSD callbacks to these networks.
	AllowedIPs []netip.Prefix
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
			// A redirected POST would be replayed as a GET; treat it as a
			// misconfigured base URL instead of following it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *Provider) Name() string { return Name }

// ---------------------------------------------------------------------------
// USSD
// ---------------------------------------------------------------------------

// USSDPayload is Arkesel's USSD session callback body. String fields also
// accept JSON numbers, since the docs show IDs and numbers that are numeric.
type USSDPayload struct {
	SessionID  flexString `json:"sessionID"`
	UserID     flexString `json:"userID"`
	NewSession bool       `json:"newSession"`
	MSISDN     flexString `json:"msisdn"`
	UserData   flexString `json:"userData"`
	Network    flexString `json:"network"`
}

// USSDResponse is what Arkesel expects back. sessionID, userID and msisdn
// must echo the request exactly; continueSession=false ends the session.
type USSDResponse struct {
	SessionID       string `json:"sessionID"`
	UserID          string `json:"userID"`
	MSISDN          string `json:"msisdn"`
	Message         string `json:"message"`
	ContinueSession bool   `json:"continueSession"`
}

func (p *Provider) ParseSession(r *http.Request, body []byte) (channel.USSDRequest, error) {
	if err := p.authenticateUSSD(r); err != nil {
		return channel.USSDRequest{}, err
	}
	var u USSDPayload
	if err := json.Unmarshal(body, &u); err != nil || u.SessionID == "" || u.MSISDN == "" {
		return channel.USSDRequest{}, fmt.Errorf("%w: need sessionID and msisdn", channel.ErrBadPayload)
	}
	input := string(u.UserData)
	if u.NewSession {
		input = "" // the dialled code, not a menu entry
	}
	// ServiceCode stays empty: Arkesel sends the dialled code only on the
	// first step, so the engine's configured USSD_SERVICE_CODE keeps every
	// step on the same conversation.
	return channel.USSDRequest{
		Provider:   Name,
		SessionID:  sessionKey(string(u.UserID), string(u.SessionID)),
		FromE164:   string(u.MSISDN), // as received; echoed back verbatim
		Input:      input,
		NewSession: u.NewSession,
		Network:    strings.ToLower(string(u.Network)),
	}, nil
}

func (p *Provider) WriteReply(w http.ResponseWriter, req channel.USSDRequest, reply channel.USSDReply) error {
	userID, sessionID := splitSessionKey(req.SessionID)
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(USSDResponse{
		SessionID: sessionID, UserID: userID, MSISDN: req.FromE164,
		Message: reply.Message, ContinueSession: !reply.End,
	})
}

// sessionKey folds Arkesel's userID (which every reply must echo) into the
// session key, so WriteReply needs no adapter-side state. The escaped userID
// cannot contain ':', which makes the split unambiguous.
func sessionKey(userID, sessionID string) string {
	return url.QueryEscape(userID) + ":" + sessionID
}

func splitSessionKey(key string) (userID, sessionID string) {
	esc, sid, ok := strings.Cut(key, ":")
	if !ok {
		return "", key
	}
	if u, err := url.QueryUnescape(esc); err == nil {
		return u, sid
	}
	return esc, sid
}

// authenticateUSSD enforces the shared-secret token and, when configured,
// the source-address allowlist. With no token configured every request is
// refused rather than accepted unauthenticated.
func (p *Provider) authenticateUSSD(r *http.Request) error {
	if len(p.opt.USSDTokens) == 0 {
		return fmt.Errorf("%w: ARKESEL_USSD_TOKEN is not configured", channel.ErrBadSignature)
	}
	got := r.URL.Query().Get(TokenParam)
	match := false
	for _, t := range p.opt.USSDTokens {
		if t != "" && security.ConstantTimeEqual(got, t) {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("%w: missing or wrong USSD token", channel.ErrBadSignature)
	}
	if len(p.opt.AllowedIPs) == 0 {
		return nil
	}
	ip, ok := p.clientIP(r)
	if ok {
		for _, prefix := range p.opt.AllowedIPs {
			if prefix.Contains(ip) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: source address not in ARKESEL_USSD_ALLOWED_IPS", channel.ErrBadSignature)
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
