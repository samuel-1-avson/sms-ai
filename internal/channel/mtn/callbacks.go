package mtn

import (
	"fmt"
	"net/url"
	"strings"
)

// Callback paths served by internal/httpapi for the provider named "mtn".
const (
	USSDCallbackPath = "/webhooks/ussd/mtn/session"
	DLRCallbackPath  = "/webhooks/sms/mtn/status"
)

// CallbackURL joins the public origin and a callback path, adding the shared
// secret as ?token= when one is given.
func CallbackURL(origin, path, token string) (string, error) {
	u, err := url.Parse(strings.TrimRight(origin, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("mtn: %q is not an absolute http(s) URL", origin)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if token != "" {
		u.RawQuery = url.Values{TokenParam: {token}}.Encode()
	}
	return u.String(), nil
}
