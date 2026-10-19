// Package app wires configuration, storage, providers and the engine
// together. cmd/api, cmd/worker and cmd/admin share it so every process
// runs the same image with the same setup (plan §4.3).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"smsaighana/data"
	"smsaighana/internal/ai"
	"smsaighana/internal/ai/deepinfra"
	mockai "smsaighana/internal/ai/mock"
	"smsaighana/internal/channel"
	"smsaighana/internal/channel/arkesel"
	"smsaighana/internal/channel/hubtel"
	"smsaighana/internal/channel/mnotify"
	mockch "smsaighana/internal/channel/mock"
	"smsaighana/internal/channel/mtn"
	"smsaighana/internal/config"
	"smsaighana/internal/grounding"
	"smsaighana/internal/observability"
	"smsaighana/internal/repository"
	"smsaighana/internal/security"
	"smsaighana/internal/service"
)

type App struct {
	Cfg       *config.Config
	Log       *slog.Logger
	Store     *repository.Store
	Registry  *channel.Registry
	Metrics   *observability.Metrics
	AI        *ai.Client
	Engine    *service.Engine
	MockGW    *mockch.Provider // nil in production
	MockModel *mockai.Model    // nil unless LLM_PROVIDER=mock
}

// Build connects to the database, applies migrations when enabled and
// loads the grounding store.
func Build(ctx context.Context, cfg *config.Config) (*App, error) {
	log := observability.NewLogger(cfg.LogLevel)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	if cfg.InsecureDevKeys {
		log.Warn("using INSECURE built-in development encryption keys; run `admin genkey` before storing real data")
	}
	kr, err := security.NewKeyring(cfg.CurrentKeyVersion, cfg.EncryptionKeys, cfg.HMACKeys)
	if err != nil {
		return nil, err
	}
	store, err := openWithRetry(ctx, cfg.DatabaseURL, kr, log)
	if err != nil {
		return nil, err
	}
	if cfg.AutoMigrate {
		applied, err := store.Migrate(ctx)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
		if len(applied) > 0 {
			log.Info("migrations applied", "versions", applied)
		}
	}

	a := &App{Cfg: cfg, Log: log, Store: store, Metrics: observability.NewMetrics()}
	a.Metrics.DailySpendCapUSD.Set(cfg.GlobalDailySpendCapUSD)

	a.Registry = channel.NewRegistry(cfg.PrimarySMSProvider)
	// The mock gateway (and with it the simulator endpoints) exists only when
	// it was explicitly selected. Choosing a real provider replaces it: it is
	// not even loaded alongside.
	if cfg.AllowsMock() && (cfg.PrimarySMSProvider == "mock" || cfg.PrimaryUSSDProvider == "mock") {
		a.MockGW = mockch.New(cfg.MockWebhookSecret, cfg.MockDLRCallbackURL, log)
		a.Registry.AddSMSInbound(a.MockGW)
		a.Registry.AddSender(a.MockGW)
		a.Registry.AddUSSD(a.MockGW)
	}
	// Real aggregator adapters register when selected. What each contract
	// confirms is in docs/PROVIDER_VALIDATION.md (plan §6.1); eSMS Africa and
	// Africa's Talking are not built yet.
	if cfg.PrimarySMSProvider == arkesel.Name || cfg.PrimaryUSSDProvider == arkesel.Name {
		a.registerArkesel()
	}
	if cfg.PrimarySMSProvider == hubtel.Name || cfg.PrimaryUSSDProvider == hubtel.Name {
		a.registerHubtel()
	}
	if cfg.PrimarySMSProvider == mtn.Name || cfg.PrimaryUSSDProvider == mtn.Name {
		a.registerMTN()
	}
	if cfg.PrimarySMSProvider == mnotify.Name || cfg.PrimaryUSSDProvider == mnotify.Name {
		a.registerMNotify()
	}
	if _, err := a.Registry.SenderFor(cfg.PrimarySMSProvider); err != nil {
		store.Close()
		return nil, fmt.Errorf("PRIMARY_SMS_PROVIDER=%q has no adapter in this build", cfg.PrimarySMSProvider)
	}
	if _, ok := a.Registry.USSD(cfg.PrimaryUSSDProvider); !ok {
		store.Close()
		return nil, fmt.Errorf("PRIMARY_USSD_PROVIDER=%q has no adapter in this build", cfg.PrimaryUSSDProvider)
	}

	var model ai.ChatModel
	switch cfg.LLMProvider {
	case "deepinfra":
		model = deepinfra.New(cfg.DeepInfraBaseURL, cfg.DeepInfraAPIKey)
	case "mock":
		if !cfg.AllowsMock() { // Validate already rejects this; belt and braces
			store.Close()
			return nil, fmt.Errorf("LLM_PROVIDER=mock is not allowed when APP_ENV=%s", cfg.Env)
		}
		a.MockModel = mockai.New(cfg.MockLLMLatency)
		model = a.MockModel
	default:
		store.Close()
		return nil, fmt.Errorf("LLM_PROVIDER=%q has no adapter in this build", cfg.LLMProvider)
	}
	a.AI = ai.NewClient(model, ai.ClientOptions{
		Primary: cfg.LLMModel, Fallback: cfg.LLMFallbackModel, ReasoningEffort: cfg.LLMReasoningEffort,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, Temperature: cfg.LLMTemperature, Prices: cfg.LLMPrices,
		Observer: a.Metrics,
	})

	a.Engine = service.New(service.Deps{
		Cfg: cfg, Store: store, Registry: a.Registry, AI: a.AI,
		Matcher: grounding.NewMatcher(nil), Metrics: a.Metrics, Log: log,
	})
	seed, err := a.seed()
	if err != nil {
		store.Close()
		return nil, err
	}
	if err := a.Engine.LoadGrounding(ctx, seed); err != nil {
		store.Close()
		return nil, fmt.Errorf("loading grounding store: %w", err)
	}
	return a, nil
}

// registerArkesel adds Arkesel as the MT SMS sender (with its delivery-report
// parser) and/or the USSD gateway, whichever roles the configuration gives it.
func (a *App) registerArkesel() {
	cfg := a.Cfg
	ark := arkesel.New(arkesel.Options{
		APIKey: cfg.ArkeselAPIKey, BaseURL: cfg.ArkeselBaseURL, CallbackURL: cfg.ArkeselDLRCallbackURL,
		Sandbox: cfg.SMSEnvironment != "live", WebhookSecret: cfg.ArkeselWebhookSecret,
		USSDTokens: cfg.ArkeselUSSDTokens, AllowedIPs: cfg.ArkeselUSSDAllowedIPs, ClientIPHeader: cfg.ArkeselClientIPHeader,
		Log: a.Log,
	})
	if cfg.PrimarySMSProvider == arkesel.Name {
		a.Registry.AddSender(ark)
		a.Registry.AddSMSInbound(ark)
		if cfg.SMSEnvironment != "live" {
			a.Log.Warn("Arkesel sandbox mode: messages are accepted but neither delivered nor billed; set SMS_ENVIRONMENT=live to send")
		}
		if cfg.ArkeselDLRCallbackURL == "" {
			a.Log.Warn("ARKESEL_DLR_CALLBACK_URL is not set; Arkesel messages will stay 'submitted' without delivery receipts")
		}
	}
	if cfg.PrimaryUSSDProvider == arkesel.Name {
		a.Registry.AddUSSD(ark)
	}
}

// registerHubtel adds Hubtel as the MT SMS sender (with its delivery-report
// parser) and/or the USSD gateway, whichever roles the configuration gives it.
func (a *App) registerHubtel() {
	cfg := a.Cfg
	hub := hubtel.New(hubtel.Options{
		ClientID: cfg.HubtelClientID, ClientSecret: cfg.HubtelClientSecret, BaseURL: cfg.HubtelSMSBaseURL,
		USSDTokens: cfg.HubtelUSSDTokens, DLRTokens: cfg.HubtelDLRTokens,
		USSDAllowedIPs: cfg.HubtelUSSDAllowedIPs, DLRAllowedIPs: cfg.HubtelDLRAllowedIPs,
		ClientIPHeader: cfg.HubtelClientIPHeader, Log: a.Log,
	})
	if cfg.PrimarySMSProvider == hubtel.Name {
		a.Registry.AddSender(hub)
		a.Registry.AddSMSInbound(hub)
		a.Log.Warn("Hubtel has no sandbox mode: every message is sent to the real network and billed")
		if len(cfg.HubtelDLRTokens) == 0 {
			a.Log.Warn("HUBTEL_DLR_TOKEN is not set; Hubtel delivery reports are refused and messages will stay 'submitted'")
		}
	}
	if cfg.PrimaryUSSDProvider == hubtel.Name {
		a.Registry.AddUSSD(hub)
	}
}

// registerMNotify adds mNotify as the MT SMS sender and/or the USSD gateway,
// whichever roles the configuration gives it. mNotify is the chosen provider;
// MTN, Hubtel and Arkesel stay available but dormant. Its SMSInbound side
// refuses everything (mNotify documents no webhooks), and the provider is
// deliberately not a channel.StatusQuerier: a send has no client reference, so
// an uncertain one cannot be looked up and must wait for an operator.
func (a *App) registerMNotify() {
	cfg := a.Cfg
	m := mnotify.New(mnotify.OptionsFromConfig(cfg, a.Log))
	if cfg.PrimarySMSProvider == mnotify.Name {
		a.Registry.AddSender(m)
		a.Registry.AddSMSInbound(m)
		a.Log.Warn("mNotify has no sandbox mode: every message is sent to the real network and billed")
		a.Log.Warn("mNotify documents no delivery-report webhook; messages stay 'submitted'")
	}
	if cfg.PrimaryUSSDProvider == mnotify.Name {
		a.Registry.AddUSSD(m)
		if len(cfg.MNotifyUSSDAllowedIPs) == 0 {
			a.Log.Warn("MNOTIFY_USSD_ALLOWED_IPS is not set; mNotify USSD callbacks are authenticated by the ?token= secret alone")
		}
	}
}

// registerMTN adds MTN's developer platform as the MT SMS sender (with its
// delivery-notification parser) and/or the USSD gateway, whichever roles the
// configuration gives it. MTN was the first choice; the owner dropped it for
// mNotify after two OAuth 401s and company-only production vetting. The code
// stays, dormant.
func (a *App) registerMTN() {
	cfg := a.Cfg
	m := mtn.New(mtn.OptionsFromConfig(cfg, a.Log))
	if cfg.PrimarySMSProvider == mtn.Name {
		a.Registry.AddSender(m.Sender())
		a.Registry.AddSMSInbound(m)
		if cfg.SMSEnvironment == "live" {
			a.Log.Warn("MTN SMS is live: every message is sent to the real network and billed")
		} else {
			a.Log.Warn("MTN SMS is pointed at a sandbox host (SMS_ENVIRONMENT=sandbox); nothing is delivered to real handsets")
		}
		if len(cfg.MTNDLRTokens) == 0 {
			a.Log.Warn("MTN_DLR_TOKEN is not set; MTN delivery notifications are refused and messages will stay 'submitted'")
		}
	}
	if cfg.PrimaryUSSDProvider == mtn.Name {
		a.Registry.AddUSSD(m)
	}
}

// seed returns snippets to upsert at start-up: always in development, and
// elsewhere only when GROUNDING_SEED_PATH is set explicitly.
func (a *App) seed() ([]grounding.Snippet, error) {
	if a.Cfg.GroundingSeedPath != "" {
		return grounding.LoadSeedFile(a.Cfg.GroundingSeedPath)
	}
	if a.Cfg.IsDevelopment() || a.Cfg.Env == "test" {
		return grounding.ParseSeed(data.GroundingSeed)
	}
	return nil, nil
}

func (a *App) Close() { a.Store.Close() }

func openWithRetry(ctx context.Context, url string, kr *security.Keyring, log *slog.Logger) (*repository.Store, error) {
	var lastErr error
	for attempt := 1; attempt <= 15; attempt++ {
		store, err := repository.Open(ctx, url, kr)
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = store.Ping(pctx)
			cancel()
			if err == nil {
				return store, nil
			}
			store.Close()
		}
		lastErr = err
		log.Warn("database not ready; retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("connecting to database: %w", lastErr)
}
