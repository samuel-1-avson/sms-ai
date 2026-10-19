package mnotify

import (
	"log/slog"

	"smsaighana/internal/config"
)

// OptionsFromConfig maps the MNOTIFY_* settings to adapter options. The serving
// path (internal/app) and the admin commands use the same mapping so they
// cannot disagree about the key or the host.
func OptionsFromConfig(cfg *config.Config, log *slog.Logger) Options {
	return Options{
		APIKey: cfg.MNotifyAPIKey, BaseURL: cfg.MNotifyBaseURL, USSDMode: cfg.MNotifyUSSDMode,
		USSDTokens: cfg.MNotifyUSSDTokens, USSDAllowedIPs: cfg.MNotifyUSSDAllowedIPs,
		ClientIPHeader: cfg.MNotifyClientIPHeader, Log: log,
	}
}
