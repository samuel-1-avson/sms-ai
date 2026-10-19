package mtn

import (
	"log/slog"

	"smsaighana/internal/config"
)

// OptionsFromConfig maps the MTN_* settings to adapter options. The serving
// path (internal/app) and the admin subscription commands use the same
// mapping so they cannot disagree about credentials or hosts.
func OptionsFromConfig(cfg *config.Config, log *slog.Logger) Options {
	return Options{
		ClientID: cfg.MTNClientID, ClientSecret: cfg.MTNClientSecret, APIKey: cfg.MTNAPIKey,
		BaseURL: cfg.MTNBaseURL, TokenURL: cfg.MTNTokenURL,
		USSDServiceCode: cfg.MTNUSSDServiceCode, SenderID: cfg.SMSOutboundSenderID, CountryCode: cfg.MTNCountryCode,
		SMSSendsAPIKey: cfg.MTNSMSSendsAPIKey, USSDSendsBearer: cfg.MTNUSSDSendsBearer, LookupByClientRef: cfg.MTNSMSLookupByClientRef,
		USSDInputIsHistory: cfg.MTNUSSDInputIsHistory,
		USSDTokens:         cfg.MTNUSSDTokens, DLRTokens: cfg.MTNDLRTokens,
		USSDAllowedIPs: cfg.MTNUSSDAllowedIPs, DLRAllowedIPs: cfg.MTNDLRAllowedIPs,
		ClientIPHeader: cfg.MTNClientIPHeader, Log: log,
	}
}
