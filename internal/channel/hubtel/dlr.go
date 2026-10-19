package hubtel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"smsaighana/internal/channel"
)

// statusMap normalizes Hubtel delivery statuses. The first eight come from
// Hubtel's delivery-status meanings table (Rejected, Delivered, Undeliverable,
// Pending, Sent, Failed, Acknowledged, Reprocessing); the rest are defensive.
// Keys are lower-case with spaces, dashes and underscores removed.
var statusMap = map[string]string{
	"delivered":     "delivered",
	"sent":          "submitted",
	"pending":       "submitted",
	"acknowledged":  "submitted",
	"reprocessing":  "submitted",
	"submitted":     "submitted",
	"queued":        "submitted",
	"accepted":      "submitted",
	"undeliverable": "failed",
	"undelivered":   "failed",
	"notdelivered":  "failed",
	"failed":        "failed",
	"rejected":      "rejected",
	"blacklisted":   "rejected",
	"expired":       "expired",
}

// mapStatus returns the normalized status and, for anything but a submitted
// or delivered message, the raw status (upper-case) as the error code.
func mapStatus(raw string) (status, errorCode string) {
	key := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(raw)))
	code := strings.ToUpper(strings.TrimSpace(raw))
	st, ok := statusMap[key]
	switch {
	case !ok:
		return "unknown", code
	case st == "delivered" || st == "submitted":
		return st, ""
	}
	return st, code
}

// ParseStatus authenticates and parses a delivery report. Hubtel does not
// sign callbacks, so the registered URL carries a token (see the package
// comment).
//
// The report format is not documented in the material that could be read, so
// the parser accepts the plausible shapes: a JSON object, a JSON array of
// objects or an object wrapping them in Data, and form or query parameters.
// Field names are matched case-insensitively. Each report needs a MessageId
// (the id returned by the send call) and a Status.
func (p *Provider) ParseStatus(r *http.Request, body []byte) ([]channel.DeliveryReceipt, error) {
	if err := p.authenticate(r, p.opt.DLRTokens, p.opt.DLRAllowedIPs, "HUBTEL_DLR_TOKEN"); err != nil {
		return nil, err
	}
	reports, err := reportFields(r, body)
	if err != nil {
		return nil, err
	}
	var out []channel.DeliveryReceipt
	for _, f := range reports {
		id := first(f, "messageid", "message_id")
		raw := first(f, "status", "messagestatus", "deliverystatus")
		if id == "" || len(id) > maxMessageIDLen || raw == "" {
			continue
		}
		st, code := mapStatus(raw)
		if st == "unknown" {
			p.log.Warn("hubtel delivery status not mapped; receipt ignored", "status", truncate(code))
		}
		out = append(out, channel.DeliveryReceipt{Provider: Name, ProviderMessageID: id, Status: st, ErrorCode: code, At: p.now()})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: need MessageId and Status", channel.ErrBadPayload)
	}
	return out, nil
}

// reportFields flattens the callback into one lower-case key/value map per
// report: query parameters first (the token among them is never read back),
// else a JSON body, else a form-encoded body.
func reportFields(r *http.Request, body []byte) ([]map[string]string, error) {
	if q := r.URL.Query(); len(q) > 0 {
		m := lowerKeys(q)
		if first(m, "messageid", "message_id") != "" {
			return []map[string]string{m}, nil
		}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty delivery report", channel.ErrBadPayload)
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		vals, err := url.ParseQuery(string(trimmed))
		if err != nil {
			return nil, fmt.Errorf("%w: unreadable delivery report", channel.ErrBadPayload)
		}
		return []map[string]string{lowerKeys(vals)}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: unreadable delivery report", channel.ErrBadPayload)
	}
	var out []map[string]string
	collect(v, &out, 0)
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no delivery report found", channel.ErrBadPayload)
	}
	return out, nil
}

// collect walks a decoded JSON value: an object with a message id is a report,
// any other object or list is searched one level deeper (Data, Messages ...).
func collect(v any, out *[]map[string]string, depth int) {
	if depth > 3 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collect(e, out, depth+1)
		}
	case map[string]any:
		flat := make(map[string]string, len(x))
		for k, e := range x {
			switch s := e.(type) {
			case string:
				flat[strings.ToLower(k)] = s
			case json.Number:
				flat[strings.ToLower(k)] = s.String()
			}
		}
		if first(flat, "messageid", "message_id") != "" {
			*out = append(*out, flat)
			return
		}
		for _, e := range x {
			switch e.(type) {
			case []any, map[string]any:
				collect(e, out, depth+1)
			}
		}
	}
}

func lowerKeys(vals url.Values) map[string]string {
	m := make(map[string]string, len(vals))
	for k, v := range vals {
		if len(v) > 0 {
			m[strings.ToLower(k)] = v[len(v)-1]
		}
	}
	return m
}

func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}
