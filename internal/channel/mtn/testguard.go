package mtn

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// refuseInTests stops a test binary from ever sending a request to MTN's real
// API hosts. The tests use httptest servers; this makes a forgotten base URL
// fail loudly instead of reaching the network.
func refuseInTests(rawURL string) error {
	if !testing.Testing() {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	if host := strings.ToLower(u.Hostname()); host == "mtn.com" || strings.HasSuffix(host, ".mtn.com") {
		return errors.New("mtn: refusing to call an MTN host from a test binary")
	}
	return nil
}
