package arkesel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"smsaighana/internal/channel"
)

const maxResponseBody = 64 << 10

type sendRequest struct {
	Sender      string   `json:"sender"`
	Message     string   `json:"message"`
	Recipients  []string `json:"recipients"`
	CallbackURL string   `json:"callback_url,omitempty"`
	Sandbox     bool     `json:"sandbox,omitempty"`
}

// apiResponse is the envelope of every SMS v2 response.
type apiResponse struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// sentItem is one element of a send response's data list: either an
// accepted recipient with its message UUID or the list of invalid numbers.
type sentItem struct {
	Recipient string   `json:"recipient"`
	ID        string   `json:"id"`
	Invalid   []string `json:"invalid numbers"`
}

// Send submits one MT SMS via POST /api/v2/sms/send. Errors are classified
// for the dispatcher: Retryable only when Arkesel certainly did not accept
// the message, Uncertain when it may have, Permanent for rejections.
func (p *Provider) Send(ctx context.Context, msg channel.OutboundSMS) (channel.SendResult, error) {
	to := strings.TrimPrefix(msg.To, "+")
	switch {
	case msg.SenderID == "" || utf8.RuneCountInString(msg.SenderID) > MaxSenderIDLen:
		return channel.SendResult{}, sendErr(channel.SendPermanent, "sender ID must be 1-%d characters", MaxSenderIDLen)
	case !allDigits(to):
		return channel.SendResult{}, sendErr(channel.SendPermanent, "recipient is not an E.164 number")
	case msg.Body == "":
		return channel.SendResult{}, sendErr(channel.SendPermanent, "empty message")
	}
	body, err := json.Marshal(sendRequest{
		Sender: msg.SenderID, Message: msg.Body, Recipients: []string{to},
		CallbackURL: p.opt.CallbackURL, Sandbox: p.opt.Sandbox,
	})
	if err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/v2/sms/send", bytes.NewReader(body))
	if err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	status, raw, written, err := p.do(req)
	if status == 0 {
		// No response. A request that never fully left cannot have been
		// accepted; one that did may have been.
		kind := channel.SendRetryable
		if written {
			kind = channel.SendUncertain
		}
		return channel.SendResult{}, &channel.SendError{Kind: kind, Err: fmt.Errorf("arkesel: %w", err)}
	}
	switch {
	case status >= 200 && status < 300:
		if err != nil {
			return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d but reading the response failed: %v", status, err)
		}
		return accepted(to, status, raw)
	case status >= 500:
		// "SMS request failed!" does not prove nothing was queued.
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d: %s", status, detail(raw))
	case notAcceptedStatus(status):
		return channel.SendResult{}, sendErr(channel.SendRetryable, "HTTP %d: %s", status, detail(raw))
	default:
		return channel.SendResult{}, sendErr(channel.SendPermanent, "HTTP %d: %s", status, detail(raw))
	}
}

// notAcceptedStatus lists responses that certainly mean "not accepted" for
// reasons outside the message itself (credentials, balance, gateway state,
// rate limits, a wrong base URL), so a later retry may succeed.
func notAcceptedStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 300 && status < 400
}

func accepted(to string, status int, raw []byte) (channel.SendResult, error) {
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with an unreadable body", status)
	}
	if !strings.EqualFold(ar.Status, "success") {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with status %q: %s", status, ar.Status, truncate(ar.Message))
	}
	var items []sentItem
	if err := json.Unmarshal(ar.Data, &items); err != nil {
		var one sentItem
		if json.Unmarshal(ar.Data, &one) != nil {
			return channel.SendResult{}, sendErr(channel.SendUncertain, "accepted without a readable message id")
		}
		items = []sentItem{one}
	}
	var ids []string
	for _, it := range items {
		for _, n := range it.Invalid {
			if strings.TrimPrefix(n, "+") == to {
				return channel.SendResult{}, sendErr(channel.SendPermanent, "recipient rejected as an invalid number")
			}
		}
		if it.ID == "" {
			continue
		}
		if strings.TrimPrefix(it.Recipient, "+") == to {
			return channel.SendResult{ProviderMessageID: it.ID}, nil
		}
		ids = append(ids, it.ID)
	}
	if len(ids) == 1 { // one recipient was sent, whatever format it is echoed in
		return channel.SendResult{ProviderMessageID: ids[0]}, nil
	}
	return channel.SendResult{}, sendErr(channel.SendUncertain, "accepted without a message id for the recipient")
}

// Lookup returns a sent message's current status by Arkesel's message UUID
// (GET /api/v2/sms/{id}). See the package comment for why this is not a
// channel.StatusQuerier.
func (p *Provider) Lookup(ctx context.Context, providerMessageID string) (channel.DeliveryReceipt, bool, error) {
	if providerMessageID == "" {
		return channel.DeliveryReceipt{}, false, errors.New("arkesel: lookup needs Arkesel's message id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/v2/sms/"+url.PathEscape(providerMessageID), nil)
	if err != nil {
		return channel.DeliveryReceipt{}, false, err
	}
	status, raw, _, err := p.do(req)
	switch {
	case err != nil:
		return channel.DeliveryReceipt{}, false, fmt.Errorf("arkesel lookup: %w", err)
	case status == http.StatusNotFound:
		return channel.DeliveryReceipt{}, false, nil
	case status != http.StatusOK:
		return channel.DeliveryReceipt{}, false, fmt.Errorf("arkesel lookup: HTTP %d: %s", status, detail(raw))
	}
	var ar struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		return channel.DeliveryReceipt{}, false, fmt.Errorf("arkesel lookup: unreadable response: %w", err)
	}
	if !strings.EqualFold(ar.Status, "success") {
		if strings.Contains(strings.ToLower(ar.Message), "not exist") {
			return channel.DeliveryReceipt{}, false, nil
		}
		return channel.DeliveryReceipt{}, false, fmt.Errorf("arkesel lookup: status %q: %s", ar.Status, truncate(ar.Message))
	}
	st, code := mapStatus(ar.Data.Status)
	return channel.DeliveryReceipt{Provider: Name, ProviderMessageID: providerMessageID, Status: st, ErrorCode: code, At: p.now()}, true, nil
}

// do performs an authenticated API call. written reports whether the
// request was completely sent before any transport error, which decides
// between retryable and uncertain failures. status is 0 without a response.
func (p *Provider) do(req *http.Request) (status int, body []byte, written bool, err error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	req.Header.Set("api-key", p.opt.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, wrote.Load(), err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, body, true, err
}

// ---------------------------------------------------------------------------
// Delivery reports
// ---------------------------------------------------------------------------

// statusMap normalizes Arkesel delivery statuses. The docs list DELIVERED,
// SUBMITTED, PROHIBITED, QUEUED, NOT_DELIVERED and EXPIRED "such as"; the
// other entries are defensive.
var statusMap = map[string]string{
	"DELIVERED":     "delivered",
	"SUBMITTED":     "submitted",
	"QUEUED":        "submitted",
	"SENT":          "submitted",
	"ACCEPTED":      "submitted",
	"NOT_DELIVERED": "failed",
	"UNDELIVERED":   "failed",
	"FAILED":        "failed",
	"PROHIBITED":    "rejected",
	"REJECTED":      "rejected",
	"EXPIRED":       "expired",
}

// mapStatus returns the normalized status and, for anything but a
// submitted or delivered message, Arkesel's raw status as the error code.
func mapStatus(raw string) (status, errorCode string) {
	key := strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(raw)), " ", "_")
	st, ok := statusMap[key]
	switch {
	case !ok:
		return "unknown", key
	case st == "delivered" || st == "submitted":
		return st, ""
	}
	return st, key
}

// ParseStatus authenticates and parses a delivery report. Arkesel appends
// sms_id and status to the callback URL's query string and signs them.
func (p *Provider) ParseStatus(r *http.Request, body []byte) ([]channel.DeliveryReceipt, error) {
	if p.opt.WebhookSecret == "" {
		return nil, fmt.Errorf("%w: ARKESEL_WEBHOOK_SECRET is not configured", channel.ErrBadSignature)
	}
	payload, err := callbackPayload(r, body)
	if err != nil {
		return nil, err
	}
	signedAt, err := Verify(p.opt.WebhookSecret, r.Header.Get(HeaderSignature), r.Header.Get(HeaderTimestamp), payload, p.now())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", channel.ErrBadSignature, err)
	}
	id, _ := payload["sms_id"].(string)
	raw, _ := payload["status"].(string)
	if id == "" || raw == "" {
		return nil, fmt.Errorf("%w: need sms_id and status", channel.ErrBadPayload)
	}
	st, code := mapStatus(raw)
	if st == "unknown" {
		p.log.Warn("arkesel delivery status not mapped; receipt ignored", "status", code, "webhook_id", r.Header.Get(HeaderWebhookID))
	}
	return []channel.DeliveryReceipt{{Provider: Name, ProviderMessageID: id, Status: st, ErrorCode: code, At: signedAt}}, nil
}

// ParseInbound refuses everything: Arkesel's v2 API has no inbound (MO) SMS
// webhook, so nothing legitimate arrives on this route.
func (p *Provider) ParseInbound(r *http.Request, body []byte) ([]channel.InboundSMS, error) {
	return nil, fmt.Errorf("%w: arkesel does not deliver inbound SMS", channel.ErrBadPayload)
}

// callbackPayload rebuilds the object Arkesel signed: the query parameters
// (a repeated key keeps its last value, as PHP does) or, if the query is
// empty, a JSON object body.
func callbackPayload(r *http.Request, body []byte) (map[string]any, error) {
	if q := r.URL.Query(); len(q) > 0 {
		m := make(map[string]any, len(q))
		for k, v := range q {
			m[k] = v[len(v)-1]
		}
		return m, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || len(m) == 0 {
		return nil, fmt.Errorf("%w: need sms_id and status query parameters", channel.ErrBadPayload)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sendErr(kind channel.SendErrorKind, format string, args ...any) error {
	return &channel.SendError{Kind: kind, Err: fmt.Errorf("arkesel: "+format, args...)}
}

// detail extracts Arkesel's error message for logs and send_attempts. It
// never echoes the request, so no phone number or message text leaks.
func detail(raw []byte) string {
	var ar apiResponse
	if json.Unmarshal(raw, &ar) == nil && ar.Message != "" {
		return truncate(ar.Message)
	}
	return "no error message"
}

func truncate(s string) string {
	if len(s) <= 200 {
		return s
	}
	s = s[:200]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
