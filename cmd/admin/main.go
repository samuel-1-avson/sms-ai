// Command admin performs operator tasks:
//
//	admin genkey [-version N]           print fresh keys and tokens for .env
//	admin migrate                       apply database migrations
//	admin seed-grounding [-file path]   upsert grounding snippets (default: embedded seed)
//	admin create-pool -code X -sponsor Y -max N -grant N [-expires YYYY-MM-DD]
//	admin flag ai_enabled|sms_outbound_enabled true|false
//	admin rekey [-batch N]              re-encrypt rows to CURRENT_KEY_VERSION
//	admin usage [-day YYYY-MM-DD]       print the day's usage report as JSON
//	admin send-webhook -url URL -file payload.json
//	                                    POST a payload signed with MOCK_WEBHOOK_SECRET
//	admin mnotify-status [-check-credentials]
//	                                    show the mNotify configuration (secrets as set/NOT SET);
//	                                    with the flag, ONE call (GET /balance/sms) prints the SMS balance
//	admin mnotify-sender-status [-sender NAME]
//	                                    ONE call (POST /senderid/status): is the sender ID approved?
//	admin mtn-subscribe [-ussd] [-sms] [-dry-run]
//	                                    register the USSD and SMS delivery callbacks with MTN (dormant provider)
//	admin mtn-unsubscribe [-ussd-id ID] [-sms-id ID] [-dry-run]
//	                                    remove them again
//	admin mtn-status [-check-credentials]
//	                                    show the MTN configuration, callback URLs (token redacted)
//
// The mnotify-* and mtn-* commands need no database. They call the provider's real
// API: mnotify-status only with -check-credentials, mnotify-sender-status always,
// the mtn ones except with -dry-run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"smsaighana/data"
	"smsaighana/internal/channel/mnotify"
	mockch "smsaighana/internal/channel/mock"
	"smsaighana/internal/channel/mtn"
	"smsaighana/internal/config"
	"smsaighana/internal/grounding"
	"smsaighana/internal/repository"
	"smsaighana/internal/security"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: admin <genkey|migrate|seed-grounding|create-pool|flag|rekey|usage|send-webhook|mnotify-status|mnotify-sender-status|mtn-subscribe|mtn-unsubscribe|mtn-status> [flags]")
}

func run(cmd string, args []string) error {
	ctx := context.Background()
	switch cmd {
	case "genkey":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		version := fs.Int("version", 1, "key version to generate")
		_ = fs.Parse(args)
		fmt.Printf("CURRENT_KEY_VERSION=%d\n", *version)
		fmt.Printf("ENCRYPTION_KEYS_V%d=%s\n", *version, randB64(32))
		fmt.Printf("HMAC_KEYS_V%d=%s\n", *version, randB64(32))
		fmt.Printf("ADMIN_API_TOKEN=%s\n", randHex(24))
		fmt.Printf("METRICS_TOKEN=%s\n", randHex(24))
		fmt.Printf("MOCK_WEBHOOK_SECRET=%s\n", randHex(24))
		return nil
	case "send-webhook":
		return sendWebhook(ctx, args)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if mnotify.IsAdminCommand(cmd) {
		return mnotify.RunAdmin(ctx, cmd, args, cfg, os.Stdout)
	}
	if mtn.IsAdminCommand(cmd) {
		return mtn.RunAdmin(ctx, cmd, args, cfg, os.Stdout)
	}
	kr, err := security.NewKeyring(cfg.CurrentKeyVersion, cfg.EncryptionKeys, cfg.HMACKeys)
	if err != nil {
		return err
	}
	store, err := repository.Open(ctx, cfg.DatabaseURL, kr)
	if err != nil {
		return err
	}
	defer store.Close()

	switch cmd {
	case "migrate":
		applied, err := store.Migrate(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d migration(s): %v\n", len(applied), applied)
	case "seed-grounding":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		file := fs.String("file", "", "seed YAML (default: embedded data/ghana_grounding_seed.yaml)")
		_ = fs.Parse(args)
		var snippets []grounding.Snippet
		if *file != "" {
			snippets, err = grounding.LoadSeedFile(*file)
		} else {
			snippets, err = grounding.ParseSeed(data.GroundingSeed)
		}
		if err != nil {
			return err
		}
		n, err := store.Q().UpsertSnippets(ctx, snippets)
		if err != nil {
			return err
		}
		reviewed := 0
		for _, s := range snippets {
			if s.Reviewed {
				reviewed++
			}
		}
		fmt.Printf("upserted %d snippet(s); %d reviewed (only reviewed snippets are served outside development)\n", n, reviewed)
	case "create-pool":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		code := fs.String("code", "", "voucher code users text, e.g. STEMGH")
		sponsor := fs.String("sponsor", "", "sponsor name")
		max := fs.Int("max", 0, "total credits in the pool")
		grant := fs.Int("grant", 0, "credits granted per phone number")
		expires := fs.String("expires", "", "optional expiry date YYYY-MM-DD (UTC end of day)")
		_ = fs.Parse(args)
		p := repository.Pool{VoucherCode: *code, SponsorName: *sponsor, MaxSegments: *max, PerUserGrant: *grant}
		if *code == "" || *sponsor == "" || *max <= 0 || *grant <= 0 {
			return errors.New("create-pool needs -code, -sponsor, -max and -grant")
		}
		if *expires != "" {
			t, err := time.Parse("2006-01-02", *expires)
			if err != nil {
				return fmt.Errorf("-expires: %w", err)
			}
			t = t.Add(24*time.Hour - time.Second)
			p.ExpiresAt = &t
		}
		created, err := store.Q().CreatePool(ctx, p)
		if err != nil {
			return err
		}
		return printJSON(created)
	case "flag":
		if len(args) != 2 {
			return errors.New("usage: admin flag <ai_enabled|sms_outbound_enabled> <true|false>")
		}
		v, err := strconv.ParseBool(args[1])
		if err != nil {
			return err
		}
		if err := store.Q().SetFlag(ctx, args[0], v); err != nil {
			return err
		}
		fmt.Printf("%s=%v\n", args[0], v)
	case "rekey":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		batch := fs.Int("batch", 500, "rows per transaction")
		_ = fs.Parse(args)
		st, err := store.Rekey(ctx, *batch)
		if err != nil {
			return err
		}
		fmt.Printf("re-encrypted to v%d: users=%d messages=%d overflow=%d ussd_sessions=%d\n",
			kr.CurrentVersion(), st.Users, st.Messages, st.Overflow, st.USSDSessions)
	case "usage":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		dayStr := fs.String("day", time.Now().UTC().Format("2006-01-02"), "UTC day")
		_ = fs.Parse(args)
		day, err := time.Parse("2006-01-02", *dayStr)
		if err != nil {
			return err
		}
		rep, err := store.Q().UsageReport(ctx, day, cfg.SMSSegmentCostUSD, cfg.GlobalDailySpendCapUSD, cfg.GlobalDailySMSSegmentCap)
		if err != nil {
			return err
		}
		return printJSON(rep)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func sendWebhook(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send-webhook", flag.ExitOnError)
	url := fs.String("url", "http://localhost:8088/webhooks/sms/mock/inbound", "target URL")
	file := fs.String("file", "", "JSON payload file")
	_ = fs.Parse(args)
	if *file == "" {
		return errors.New("-file is required")
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	secret := os.Getenv("MOCK_WEBHOOK_SECRET")
	if secret == "" {
		secret = "dev-mock-webhook-secret"
	}
	req, err := mockch.SignedRequest(ctx, secret, *url, body, time.Now())
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s\n%s", resp.Status, out)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
