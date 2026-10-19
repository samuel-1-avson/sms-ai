package mnotify

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"smsaighana/internal/config"
)

// adminTimeout bounds each real API call an admin command makes.
const adminTimeout = 20 * time.Second

// RunAdmin implements `admin mnotify-status` and `admin mnotify-sender-status`.
// They are the only code that calls mNotify outside the serving path. Each
// prints the configuration with secrets shown only as set or NOT SET; the API
// key is never printed, and request URLs (which carry it) are never printed.
//
//	admin mnotify-status                     configuration only, no network call
//	admin mnotify-status -check-credentials  ONE call, GET /balance/sms: prints the credit balance
//	admin mnotify-sender-status [-sender X]  ONE call, POST /senderid/status: Pending or Approved
func RunAdmin(ctx context.Context, cmd string, args []string, cfg *config.Config, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch cmd {
	case "mnotify-status":
		check := fs.Bool("check-credentials", false, "call GET /balance/sms once to prove the API key and print the SMS balance (one real API call)")
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("usage: admin mnotify-status [-check-credentials]: %w", err)
		}
		return status(ctx, cfg, out, *check)
	case "mnotify-sender-status":
		sender := fs.String("sender", cfg.SMSOutboundSenderID, "sender ID to look up (default SMS_OUTBOUND_SENDER_ID)")
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("usage: admin mnotify-sender-status [-sender NAME]: %w", err)
		}
		return senderStatus(ctx, cfg, out, *sender)
	}
	return fmt.Errorf("unknown mNotify command %q", cmd)
}

// IsAdminCommand reports whether cmd is handled by RunAdmin.
func IsAdminCommand(cmd string) bool {
	return cmd == "mnotify-status" || cmd == "mnotify-sender-status"
}

func quietProvider(cfg *config.Config) *Provider {
	return New(OptionsFromConfig(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func status(ctx context.Context, cfg *config.Config, out io.Writer, check bool) error {
	p := quietProvider(cfg)
	base := p.api.scrub(displayURL(cfg.MNotifyBaseURL))
	yes := func(b bool) string {
		if b {
			return "set"
		}
		return "NOT SET"
	}
	fmt.Fprintf(out, "PRIMARY_SMS_PROVIDER=%s  PRIMARY_USSD_PROVIDER=%s  SMS_ENVIRONMENT=%s\n",
		cfg.PrimarySMSProvider, cfg.PrimaryUSSDProvider, cfg.SMSEnvironment)
	fmt.Fprintf(out, "API host           %s (%s)\n", base, hostLabel(cfg.MNotifyBaseURL))
	fmt.Fprintf(out, "API key            %s\n", yes(cfg.MNotifyAPIKey != ""))
	fmt.Fprintf(out, "SMS sender ID      %s\n", orNone(cfg.SMSOutboundSenderID))
	fmt.Fprintf(out, "USSD mode          %s\n", cfg.MNotifyUSSDMode)
	fmt.Fprintf(out, "USSD callback      %s?%s=<token>   (%d token(s) %s, %d allowlist entr(ies), client IP header %s)\n",
		USSDCallbackPath, TokenParam, len(cfg.MNotifyUSSDTokens), yes(len(cfg.MNotifyUSSDTokens) > 0),
		len(cfg.MNotifyUSSDAllowedIPs), orNone(cfg.MNotifyClientIPHeader))
	fmt.Fprintln(out, "delivery reports   none: mNotify documents no delivery-report webhook, so messages stay 'submitted'")
	fmt.Fprintln(out, "send lookup        none: a send has no client reference, so an uncertain send is checked in the mNotify dashboard")
	if !check {
		return nil
	}
	if cfg.MNotifyAPIKey == "" {
		return errors.New("cannot check credentials: MNOTIFY_API_KEY is not set")
	}
	fmt.Fprintf(out, "credential check   GET %s/balance/sms (one call)\n", base)
	ctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	code, raw, _, err := p.api.do(ctx, http.MethodGet, balanceSMSPath, nil, nil)
	if err != nil {
		return fmt.Errorf("credential check failed: %w", err)
	}
	var resp struct {
		Status  flexString      `json:"status"`
		Balance flexNum         `json:"balance"`
		Bonus   flexNum         `json:"bonus"`
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(raw, &resp)
	if code < 200 || code >= 300 || !strings.EqualFold(string(resp.Status), "success") || !resp.Balance.Set {
		return fmt.Errorf("credential check failed: HTTP %d: %s", code, p.cleanText(rawText(resp.Message)))
	}
	fmt.Fprintf(out, "credential check   mNotify accepted the key: SMS balance %d credits", resp.Balance.N)
	if resp.Bonus.Set {
		fmt.Fprintf(out, ", bonus %d", resp.Bonus.N)
	}
	fmt.Fprintln(out)
	if resp.Balance.N < lowCreditWarn {
		fmt.Fprintln(out, "                   LOW: top up in the mNotify dashboard before going live; at zero every send is rejected")
	}
	return nil
}

func senderStatus(ctx context.Context, cfg *config.Config, out io.Writer, sender string) error {
	sender = strings.TrimSpace(sender)
	if sender == "" || len([]rune(sender)) > MaxSenderIDLen {
		return fmt.Errorf("sender ID must be 1-%d characters", MaxSenderIDLen)
	}
	if cfg.MNotifyAPIKey == "" {
		return errors.New("cannot look up the sender ID: MNOTIFY_API_KEY is not set")
	}
	p := quietProvider(cfg)
	fmt.Fprintf(out, "sender ID lookup   POST %s%s for %q (one call)\n", p.api.scrub(displayURL(cfg.MNotifyBaseURL)), senderStatusAPI, sender)
	body, _ := json.Marshal(map[string]string{"sender_name": sender})
	ctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	code, raw, _, err := p.api.do(ctx, http.MethodPost, senderStatusAPI, nil, body)
	if err != nil {
		return fmt.Errorf("sender ID lookup failed: %w", err)
	}
	var resp struct {
		Status  flexString      `json:"status"`
		Code    flexString      `json:"code"`
		Message json.RawMessage `json:"message"`
		Summary json.RawMessage `json:"summary"`
	}
	_ = json.Unmarshal(raw, &resp)
	if code < 200 || code >= 300 || !strings.EqualFold(string(resp.Status), "success") {
		return fmt.Errorf("sender ID lookup failed: HTTP %d: %s", code, p.cleanText(rawText(resp.Message)))
	}
	state := senderState(resp.Summary)
	if state == "" {
		return fmt.Errorf("sender ID lookup: mNotify answered success but the summary has no status (%s)", p.cleanText(rawText(resp.Summary)))
	}
	fmt.Fprintf(out, "sender ID lookup   %q is %s\n", sender, p.cleanText(state))
	if !strings.EqualFold(state, "approved") {
		fmt.Fprintln(out, "                   not approved: sends with this sender ID may be rejected until mNotify approves it")
	}
	return nil
}

// senderState extracts the status from the summary of /senderid/status. The
// OpenAPI example prints the summary as {"sender name": "mNotify", "status":
// "Approved"} (note the space in the first key); any key whose name contains
// "status" is accepted.
func senderState(summary json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(summary, &m) != nil {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), "status") {
			var s flexString
			if s.UnmarshalJSON(m[k]) == nil && strings.TrimSpace(string(s)) != "" {
				return strings.TrimSpace(string(s))
			}
		}
	}
	return ""
}

// displayURL is a base URL safe to print: no query string, fragment or
// credentials, in case one was pasted into the setting.
func displayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func hostLabel(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "unparseable"
	}
	host := strings.ToLower(u.Hostname())
	if host == "api.mnotify.com" || strings.HasSuffix(host, ".mnotify.com") || host == "bms.africa" || strings.HasSuffix(host, ".bms.africa") {
		return "mNotify production: no sandbox exists"
	}
	return "not an mNotify host"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
