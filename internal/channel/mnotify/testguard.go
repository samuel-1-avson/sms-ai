package mnotify

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// refuseInTests stops a test binary from ever sending a request to mNotify's
// real API hosts. The tests use httptest servers; this makes a forgotten base
// URL fail loudly instead of reaching the network. The error names only the
// host: never the URL, which carries the API key.
func refuseInTests(rawURL string) error {
	if !testing.Testing() {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range []string{"mnotify.com", "bms.africa"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return errors.New("mnotify: refusing to call an mNotify host (" + host + ") from a test binary")
		}
	}
	return nil
}
