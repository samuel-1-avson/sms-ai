package mtn

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"smsaighana/internal/config"
)

// RunAdmin implements `admin mtn-subscribe`, `admin mtn-unsubscribe` and
// `admin mtn-status`. They are the only code that calls MTN's subscription
// APIs; each prints what it does with callback tokens redacted and never
// prints a credential. -dry-run prints the requests without sending them.
//
// MTN's Swagger files have no operation that lists subscriptions, so
// mtn-status reports the local configuration and the subscription ids the
// operator recorded in MTN_USSD_SUBSCRIPTION_ID and MTN_SMS_SUBSCRIPTION_ID
// after mtn-subscribe printed them.
func RunAdmin(ctx context.Context, cmd string, args []string, cfg *config.Config, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch cmd {
	case "mtn-subscribe":
		ussd := fs.Bool("ussd", false, "register only the USSD callback")
		sms := fs.Bool("sms", false, "register only the SMS delivery-notification callback")
		dry := fs.Bool("dry-run", false, "print the requests without sending them")
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("usage: admin mtn-subscribe [-ussd] [-sms] [-dry-run]: %w", err)
		}
		both := !*ussd && !*sms
		return subscribe(ctx, cfg, out, *ussd || both, *sms || both, *dry)
	case "mtn-unsubscribe":
		ussdID := fs.String("ussd-id", cfg.MTNUSSDSubscriptionID, "USSD subscription id (default MTN_USSD_SUBSCRIPTION_ID)")
		smsID := fs.String("sms-id", cfg.MTNSMSSubscriptionID, "SMS subscription id (default MTN_SMS_SUBSCRIPTION_ID)")
		dry := fs.Bool("dry-run", false, "print the requests without sending them")
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("usage: admin mtn-unsubscribe [-ussd-id ID] [-sms-id ID] [-dry-run]: %w", err)
		}
		return unsubscribe(ctx, cfg, out, *ussdID, *smsID, *dry)
	case "mtn-status":
		check := fs.Bool("check-credentials", false, "request an OAuth token from MTN to prove the client id and secret (one real API call)")
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("usage: admin mtn-status [-check-credentials]: %w", err)
		}
		return status(ctx, cfg, out, *check)
	}
	return fmt.Errorf("unknown MTN command %q", cmd)
}

// IsAdminCommand reports whether cmd is handled by RunAdmin.
func IsAdminCommand(cmd string) bool {
	return cmd == "mtn-subscribe" || cmd == "mtn-unsubscribe" || cmd == "mtn-status"
}

func quietOptions(cfg *config.Config) Options {
	return OptionsFromConfig(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func firstToken(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func subscribe(ctx context.Context, cfg *config.Config, out io.Writer, ussd, sms, dry bool) error {
	if err := cfg.ValidateMTNRegistration(ussd, sms); err != nil {
		return fmt.Errorf("cannot register callbacks:\n%w", err)
	}
	reg := NewRegistrar(quietOptions(cfg))
	var recorded []string
	var failures []error

	if ussd {
		cb, err := CallbackURL(cfg.MTNCallbackBaseURL, USSDCallbackPath, firstToken(cfg.MTNUSSDTokens))
		if err != nil {
			return err
		}
		sub := USSDSubscription{ServiceCode: cfg.MTNUSSDServiceCode, CallbackURL: cb, TargetSystem: cfg.MTNTargetSystem}
		fmt.Fprintf(out, "USSD: POST %s%s\n  serviceCode  %s\n  callbackUrl  %s\n  targetSystem %s\n",
			reg.api.base, ussdSubscriptionAPI, sub.ServiceCode, RedactURL(sub.CallbackURL), sub.TargetSystem)
		if !dry {
			id, err := reg.SubscribeUSSD(ctx, sub)
			if err != nil {
				failures = append(failures, fmt.Errorf("USSD: %w", err))
			} else {
				fmt.Fprintf(out, "  -> USSD subscription id: %s\n", id)
				recorded = append(recorded, "MTN_USSD_SUBSCRIPTION_ID="+id)
			}
		}
	}
	if sms {
		dlr, err := CallbackURL(cfg.MTNCallbackBaseURL, DLRCallbackPath, firstToken(cfg.MTNDLRTokens))
		if err != nil {
			return err
		}
		sub := SMSSubscription{SenderAddress: cfg.SMSOutboundSenderID, NotifyURL: dlr, TargetSystem: cfg.MTNTargetSystem}
		fmt.Fprintf(out, "SMS: POST %s%s\n  x-country-code %s\n  notifyUrl      %s\n  targetSystem   %s\n",
			reg.api.base, smsSubscriptionPath(sub.SenderAddress), reg.smsHeaders()["x-country-code"], RedactURL(sub.NotifyURL), sub.TargetSystem)
		if !dry {
			id, err := reg.SubscribeSMS(ctx, sub)
			if err != nil {
				failures = append(failures, fmt.Errorf("SMS: %w", err))
			} else {
				fmt.Fprintf(out, "  -> SMS subscription id: %s\n", id)
				recorded = append(recorded, "MTN_SMS_SUBSCRIPTION_ID="+id)
			}
		}
	}
	if dry {
		fmt.Fprintln(out, "Dry run: nothing was sent to MTN.")
	}
	if len(recorded) > 0 {
		fmt.Fprintln(out, "\nKeep these lines in your env file (ids are not secrets); mtn-unsubscribe reads them:")
		for _, l := range recorded {
			fmt.Fprintln(out, l)
		}
	}
	return errors.Join(failures...)
}

func unsubscribe(ctx context.Context, cfg *config.Config, out io.Writer, ussdID, smsID string, dry bool) error {
	ussdID, smsID = strings.TrimSpace(ussdID), strings.TrimSpace(smsID)
	if ussdID == "" && smsID == "" {
		return errors.New("nothing to remove: pass -ussd-id and/or -sms-id, or set MTN_USSD_SUBSCRIPTION_ID / MTN_SMS_SUBSCRIPTION_ID (MTN offers no way to list subscriptions)")
	}
	// Only the credentials matter when removing; the callback origin and
	// tokens are not needed.
	var need []string
	if ussdID != "" && cfg.MTNAPIKey == "" && cfg.MTNClientID == "" {
		need = append(need, "MTN_API_KEY or MTN_CLIENT_ID")
	}
	if smsID != "" && (cfg.MTNClientID == "" || cfg.MTNClientSecret == "") {
		need = append(need, "MTN_CLIENT_ID and MTN_CLIENT_SECRET")
	}
	if smsID != "" && cfg.SMSOutboundSenderID == "" {
		need = append(need, "SMS_OUTBOUND_SENDER_ID (the subscription belongs to a sender address)")
	}
	if len(need) > 0 {
		return fmt.Errorf("cannot remove callbacks: %s required", strings.Join(need, " and "))
	}
	reg := NewRegistrar(quietOptions(cfg))
	var failures []error
	if ussdID != "" {
		fmt.Fprintf(out, "USSD: DELETE %s%s/%s\n", reg.api.base, ussdSubscriptionAPI, ussdID)
		if !dry {
			if err := reg.UnsubscribeUSSD(ctx, ussdID); err != nil {
				failures = append(failures, fmt.Errorf("USSD: %w", err))
			} else {
				fmt.Fprintln(out, "  -> removed")
			}
		}
	}
	if smsID != "" {
		fmt.Fprintf(out, "SMS: DELETE %s%s/%s\n", reg.api.base, smsSubscriptionPath(cfg.SMSOutboundSenderID), smsID)
		if !dry {
			if err := reg.UnsubscribeSMS(ctx, cfg.SMSOutboundSenderID, smsID); err != nil {
				failures = append(failures, fmt.Errorf("SMS: %w", err))
			} else {
				fmt.Fprintln(out, "  -> removed")
			}
		}
	}
	if dry {
		fmt.Fprintln(out, "Dry run: nothing was sent to MTN.")
	}
	return errors.Join(failures...)
}

func status(ctx context.Context, cfg *config.Config, out io.Writer, check bool) error {
	yes := func(b bool) string {
		if b {
			return "set"
		}
		return "NOT SET"
	}
	fmt.Fprintf(out, "PRIMARY_SMS_PROVIDER=%s  PRIMARY_USSD_PROVIDER=%s  SMS_ENVIRONMENT=%s\n",
		cfg.PrimarySMSProvider, cfg.PrimaryUSSDProvider, cfg.SMSEnvironment)
	fmt.Fprintf(out, "API host          %s (%s)\n", cfg.MTNBaseURL, hostLabel(cfg.MTNBaseURL))
	fmt.Fprintf(out, "OAuth token URL   %s\n", cfg.MTNTokenURL)
	fmt.Fprintf(out, "client id/secret  %s / %s\n", yes(cfg.MTNClientID != ""), yes(cfg.MTNClientSecret != ""))
	fmt.Fprintf(out, "x-api-key         %s\n", apiKeySource(cfg))
	fmt.Fprintf(out, "USSD service code %s   target system %s\n", cfg.MTNUSSDServiceCode, cfg.MTNTargetSystem)
	fmt.Fprintf(out, "SMS sender        %s   x-country-code %s\n", orNone(cfg.SMSOutboundSenderID), cfg.MTNCountryCode)
	fmt.Fprintf(out, "credential knobs  SMS also sends x-api-key: %v   USSD also sends bearer token: %v   lookup by client ref: %v\n",
		cfg.MTNSMSSendsAPIKey, cfg.MTNUSSDSendsBearer, cfg.MTNSMSLookupByClientRef)
	fmt.Fprintf(out, "callback origin   %s\n", orNone(cfg.MTNCallbackBaseURL))
	for _, c := range []struct {
		name, path string
		tokens     []string
		allow      int
	}{
		{"USSD callback", USSDCallbackPath, cfg.MTNUSSDTokens, len(cfg.MTNUSSDAllowedIPs)},
		{"delivery URL ", DLRCallbackPath, cfg.MTNDLRTokens, len(cfg.MTNDLRAllowedIPs)},
	} {
		line := "-"
		if cfg.MTNCallbackBaseURL != "" {
			if u, err := CallbackURL(cfg.MTNCallbackBaseURL, c.path, firstToken(c.tokens)); err == nil {
				line = RedactURL(u)
			}
		}
		fmt.Fprintf(out, "%s %s   (%d token(s), %d allowlist entr(ies))\n", c.name, line, len(c.tokens), c.allow)
	}
	fmt.Fprintf(out, "recorded subscriptions: USSD %s, SMS %s\n", orNone(cfg.MTNUSSDSubscriptionID), orNone(cfg.MTNSMSSubscriptionID))
	fmt.Fprintln(out, "MTN has no call that lists subscriptions: ids exist only if mtn-subscribe printed them and you recorded them.")
	if !check {
		return nil
	}
	if cfg.MTNClientID == "" || cfg.MTNClientSecret == "" {
		return errors.New("cannot check credentials: MTN_CLIENT_ID and MTN_CLIENT_SECRET are not set")
	}
	if _, err := NewRegistrar(quietOptions(cfg)).Tokens().Token(ctx); err != nil {
		return fmt.Errorf("credential check failed: %w", err)
	}
	fmt.Fprintln(out, "credential check: MTN issued an access token (not shown)")
	return nil
}

func hostLabel(base string) string {
	switch {
	case strings.Contains(base, "sandbox.api.mtn.com"):
		return "MTN sandbox"
	case strings.Contains(base, "api.mtn.com"):
		return "MTN production"
	}
	return "not an MTN host"
}

func apiKeySource(cfg *config.Config) string {
	switch {
	case cfg.MTNAPIKey != "":
		return "set (MTN_API_KEY)"
	case cfg.MTNClientID != "":
		return "using MTN_CLIENT_ID (MTN's FAQ: the API key is the app's Consumer Key)"
	}
	return "NOT SET"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
