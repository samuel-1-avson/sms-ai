package mtn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"smsaighana/internal/channel"
)

// statusMap normalizes delivery statuses. MTN's SMS V2 Swagger gives the status
// as free text ("status: string"), so this covers the statuses of MTN's SMS v3
// Swagger (ACCEPTD, DELETED, DELIVERED, ENROUTE, UNKNOWN, EXPIRED, REJECTED,
// UNDELIVERED), the SMPP-style spellings, and the OMA RESTful Short Messaging
// vocabulary that V2's clientCorrelator/senderAddress/receiverAddress fields
// come from. Keys are lower-case with spaces, dashes and underscores removed.
var statusMap = map[string]string{
	"delivered":           "delivered",
	"delivrd":             "delivered",
	"deliveredtoterminal": "delivered",
	"success":             "delivered",
	"successful":          "delivered",
	"acceptd":             "submitted",
	"accepted":            "submitted",
	"enroute":             "submitted",
	"pending":             "submitted",
	"submitted":           "submitted",
	"sent":                "submitted",
	"queued":              "submitted",
	"messagewaiting":      "submitted",
	"deliveredtonetwork":  "submitted",
	"undelivered":         "failed",
	"undeliv":             "failed",
	"notdelivered":        "failed",
	"deleted":             "failed",
	"failed":              "failed",
	"deliveryimpossible":  "failed",
	"rejected":            "rejected",
	"rejectd":             "rejected",
	"expired":             "expired",
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

// notice is one delivery notification as found in a callback body.
type notice struct {
	ids       []string // requestId and clientCorrelator, whichever are present
	status    string
	errText   string
	completed string
}

// ParseStatus authenticates and parses a delivery notification. MTN does not
// sign callbacks, so the registered URL carries a token (see the package
// comment).
//
// MTN's SMS V2 Swagger documents the subscription but not the body MTN posts.
// The parser follows the delivery-status lookup's DeliveryInfo model, which is
// the most likely shape: requestId, clientCorrelator and deliveryStatus, a list
// of {receiverAddress, status}. It also accepts a flat deliveryStatus string
// (the SMS v3 notification, with clientCorrelatorId), OMA-style nesting under
// deliveryInfo, an array of notifications, and any of those under "data".
// Field names are matched case-insensitively. A notification needs a request id
// or a client reference, and a status.
//
// A notification that carries both ids yields one receipt per id: the message
// is stored under the request id MTN returned from the send call, and the
// repository applies a receipt to whichever stored id it matches (the others
// match nothing).
func (p *Provider) ParseStatus(r *http.Request, body []byte) ([]channel.DeliveryReceipt, error) {
	if err := p.authenticate(r, p.opt.DLRTokens, p.opt.DLRAllowedIPs, "MTN_DLR_TOKEN"); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, fmt.Errorf("%w: unreadable delivery notification", channel.ErrBadPayload)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: unreadable delivery notification", channel.ErrBadPayload)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing delivery notification data", channel.ErrBadPayload)
	}
	var notes []notice
	collect(v, 0, &notes)

	var out []channel.DeliveryReceipt
	for _, n := range notes {
		st, code := mapStatus(n.status)
		if st == "unknown" {
			p.log.Warn("mtn delivery status not mapped; receipt ignored", "status", truncate(code))
		}
		if st != "delivered" && st != "submitted" && n.errText != "" {
			code = clip(n.errText, 64)
		}
		code = clip(code, 64)
		at := p.receiptTime(n.completed)
		for _, id := range n.ids {
			out = append(out, channel.DeliveryReceipt{Provider: Name, ProviderMessageID: id, Status: st, ErrorCode: code, At: at})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: need a requestId or clientCorrelator and a delivery status", channel.ErrBadPayload)
	}
	return out, nil
}

// collect walks a decoded callback body: an object with an id and a status is
// a notification; any other object or list is searched a level deeper.
func collect(v any, depth int, out *[]notice) {
	if depth > 4 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collect(e, depth+1, out)
		}
	case map[string]any:
		m := lowerKeys(x)
		if ids := idsOf(m); len(ids) > 0 {
			if st := statusOf(m, 0); st != "" {
				*out = append(*out, notice{ids: ids, status: st, errText: scalar(m["error"]), completed: scalar(m["completeddate"])})
				return
			}
		}
		for _, e := range x {
			switch e.(type) {
			case []any, map[string]any:
				collect(e, depth+1, out)
			}
		}
	}
}

// idsOf returns the distinct ids of a notification object.
func idsOf(m map[string]any) []string {
	var ids []string
	for _, k := range []string{"requestid", "clientcorrelator", "clientcorrelatorid"} {
		id := scalar(m[k])
		if id == "" || len(id) > maxMessageIDLen {
			continue
		}
		dup := false
		for _, have := range ids {
			dup = dup || have == id
		}
		if !dup {
			ids = append(ids, id)
		}
	}
	return ids
}

// statusOf finds the delivery status in a notification object: deliveryStatus
// as a string, as a list of {receiverAddress, status}, or as an object; a plain
// status string; or any of those inside deliveryInfo (OMA) or data.
func statusOf(m map[string]any, depth int) string {
	if depth > 3 {
		return ""
	}
	for _, k := range []string{"deliverystatus", "status"} {
		if s := statusIn(m[k], depth); s != "" {
			return s
		}
	}
	for _, k := range []string{"deliveryinfo", "deliveryinfolist", "deliveryinfonotification", "data"} {
		if s := statusIn(m[k], depth); s != "" {
			return s
		}
	}
	return ""
}

// statusIn reads a status from a string, from the first entry of a list (one
// receiver per request is sent) or from an object.
func statusIn(v any, depth int) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		return statusOf(lowerKeys(x), depth+1)
	case []any:
		if len(x) > 0 {
			return statusIn(x[0], depth+1)
		}
	}
	return ""
}

func lowerKeys(x map[string]any) map[string]any {
	m := make(map[string]any, len(x))
	for k, e := range x {
		m[strings.ToLower(k)] = e
	}
	return m
}

// scalar renders a JSON string or number as text, and anything else as "".
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	}
	return ""
}

// receiptTime parses a completion time, whose format MTN does not state; an
// unreadable or missing value falls back to the time of arrival.
func (p *Provider) receiptTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return p.now()
}
