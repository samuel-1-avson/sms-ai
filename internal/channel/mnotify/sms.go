package mnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"smsaighana/internal/channel"
)

// lowCreditWarn is the credit_left value below which a send logs a warning.
// At zero credits mNotify rejects sends, and the dispatcher treats an
// insufficient-balance rejection as permanent, so the message is lost.
const (
	lowCreditWarn     = 100
	lowCreditInterval = 10 * time.Minute
)

// sendRequest is the JSON body of POST /sms/quick (the OpenAPI file's
// "Quick Bulk SMS"). sms_type is deliberately absent: "otp" adds a charge of
// 0.035 per campaign, and mNotify says unwanted use may cause validation
// errors. is_schedule and schedule_date are sent exactly as the examples show
// them for an immediate send.
type sendRequest struct {
	Recipient    []string `json:"recipient"`
	Sender       string   `json:"sender"`
	Message      string   `json:"message"`
	IsSchedule   bool     `json:"is_schedule"`
	ScheduleDate string   `json:"schedule_date"`
}

// sendResponse is the 200 body of /sms/quick. The only documented outcome is
// status "success" with code "2000"; the file prints no error body at all.
// summary is kept raw because an error response may carry [] or a string there.
type sendResponse struct {
	Status  flexString      `json:"status"`
	Code    flexString      `json:"code"`
	Message json.RawMessage `json:"message"`
	Summary json.RawMessage `json:"summary"`
}

// sendSummary is summary of a success: _id is the campaign id, the handle of
// mNotify's delivery lookup.
type sendSummary struct {
	ID            flexString `json:"_id"`
	TotalSent     flexNum    `json:"total_sent"`
	TotalRejected flexNum    `json:"total_rejected"`
	CreditLeft    flexNum    `json:"credit_left"`
}

func sendErr(kind channel.SendErrorKind, format string, args ...any) error {
	return &channel.SendError{Kind: kind, Err: fmt.Errorf("mnotify: "+format, args...)}
}

// Send submits one MT SMS. Errors are classified for the dispatcher:
// Retryable only when mNotify certainly did not accept the message, Uncertain
// when it may have (there is no reference to look it up by afterwards),
// Permanent for rejections of the message itself.
func (p *Provider) Send(ctx context.Context, msg channel.OutboundSMS) (channel.SendResult, error) {
	to, ok := localNumber(msg.To)
	switch {
	case msg.SenderID == "" || utf8.RuneCountInString(msg.SenderID) > MaxSenderIDLen:
		return channel.SendResult{}, sendErr(channel.SendPermanent, "sender ID must be 1-%d characters", MaxSenderIDLen)
	case !ok:
		return channel.SendResult{}, sendErr(channel.SendPermanent, "recipient is not a Ghana mobile number (+233...); the adapter sends to Ghana numbers only")
	case msg.Body == "":
		return channel.SendResult{}, sendErr(channel.SendPermanent, "empty message")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sendRequest{Recipient: []string{to}, Sender: msg.SenderID, Message: msg.Body}); err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}

	status, raw, written, err := p.api.do(ctx, http.MethodPost, smsQuickPath, nil, buf.Bytes())
	if status == 0 {
		// No response. A request that never fully left (no key configured, a
		// refused connection, a cancelled context) cannot have been accepted;
		// one that did may have been.
		kind := channel.SendRetryable
		if written {
			kind = channel.SendUncertain
		}
		return channel.SendResult{}, &channel.SendError{Kind: kind, Err: err}
	}
	switch {
	case status >= 200 && status < 300:
		if err != nil {
			return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d but reading the response failed: %v", status, err)
		}
		return p.accepted(status, raw, msg.ClientRef)
	case status >= 500:
		// A server error does not prove nothing was queued.
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d: %s", status, p.detail(raw))
	default:
		return channel.SendResult{}, p.rejected(status, raw)
	}
}

// localNumber converts our E.164 number to the local form every example of the
// OpenAPI file uses (0241234567). Only Ghana numbers are supported: the file
// shows no international recipient, and the product is Ghana-only.
func localNumber(to string) (string, bool) {
	d := strings.TrimPrefix(strings.TrimSpace(to), "+")
	switch {
	case !allDigits(d):
		return "", false
	case strings.HasPrefix(d, "233") && len(d) == 12:
		return "0" + d[3:], true
	case len(d) == 10 && d[0] == '0':
		return d, true
	}
	return "", false
}

// accepted checks a 2xx body of /sms/quick.
func (p *Provider) accepted(status int, raw []byte, clientRef string) (channel.SendResult, error) {
	var resp sendResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with an unreadable body", status)
	}
	var sum sendSummary
	_ = json.Unmarshal(resp.Summary, &sum) // [] or "" in a non-success body: leave zero
	st := strings.ToLower(strings.TrimSpace(string(resp.Status)))
	code := strings.TrimSpace(string(resp.Code))
	id := strings.TrimSpace(string(sum.ID))

	switch {
	case st == "success" && (code == "2000" || code == "" && id != ""):
		switch {
		case sum.TotalRejected.Set && sum.TotalRejected.N > 0 && !(sum.TotalSent.Set && sum.TotalSent.N > 0):
			// One recipient per call: rejected means the number was refused.
			return channel.SendResult{}, sendErr(channel.SendPermanent, "recipient rejected by mNotify (total_rejected %d)", sum.TotalRejected.N)
		case sum.TotalSent.Set && sum.TotalSent.N < 1:
			return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d, success code, but total_sent is %d", status, sum.TotalSent.N)
		}
		if sum.CreditLeft.Set {
			p.noteCredit(sum.CreditLeft.N)
		}
		if id == "" || len(id) > maxMessageIDLen {
			// The campaign id is the handle of mNotify's delivery lookup. Without
			// one the message is still accepted; our own reference stands in.
			id = clientRef
		}
		return channel.SendResult{ProviderMessageID: id}, nil
	case st == "success":
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d, status success with unexpected code %q: %s", status, truncate(code), p.messageText(resp))
	case st == "" && code == "":
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with a body that has neither status nor code", status)
	}
	// A 2xx with an explicit failure status. The file documents no error codes,
	// so the text decides; an error nobody recognises may still have queued the
	// message, so it waits for an operator.
	text := p.messageText(resp)
	if kind, why, ok := classifyText(string(resp.Code) + " " + text); ok {
		return channel.SendResult{}, sendErr(kind, "HTTP %d, status %q (%s): %s", status, truncate(st), why, text)
	}
	return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d, status %q, code %q: %s", status, truncate(st), truncate(code), text)
}

// rejected classifies a 3xx or 4xx answer: mNotify did not accept the message.
func (p *Provider) rejected(status int, raw []byte) error {
	var resp sendResponse
	_ = json.Unmarshal(raw, &resp)
	text := p.messageText(resp)
	if kind, why, ok := classifyText(string(resp.Code) + " " + text); ok {
		return sendErr(kind, "HTTP %d (%s): %s", status, why, text)
	}
	if notAcceptedStatus(status) {
		return sendErr(channel.SendRetryable, "HTTP %d: %s", status, text)
	}
	// Any other 4xx is a validation rejection of this very message; sending the
	// same payload again would fail the same way.
	return sendErr(channel.SendPermanent, "HTTP %d: %s", status, text)
}

// notAcceptedStatus lists responses that certainly mean "not accepted" for
// reasons outside the message itself (credentials, permissions, rate limits, a
// wrong base URL, a proxy asking for authentication), so a later retry may
// succeed. 3xx is here because redirects are not followed: it means the base
// URL is wrong.
func notAcceptedStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusProxyAuthRequired, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 300 && status < 400
}

// classifyText maps an mNotify message to a send error kind, as far as the
// OpenAPI file allows: it prints no error codes or texts, so this recognises
// the plain words an error about credentials, the sender ID, the balance or the
// number would use. Credential errors are retryable (nothing was sent and the
// operator can fix the key); the rest are rejections of this message.
func classifyText(s string) (kind channel.SendErrorKind, why string, ok bool) {
	t := strings.ToLower(s)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(t, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("api key", "apikey", "invalid key", "unauthor", "unauthenticated", "authenticat", "forbidden", "access denied"):
		return channel.SendRetryable, "credentials", true
	case has("sender"):
		return channel.SendPermanent, "sender ID not accepted", true
	case has("balance", "insufficient", "credit", "fund", "top up", "top-up", "wallet"):
		return channel.SendPermanent, "balance", true
	case has("invalid number", "invalid phone", "invalid recipient", "recipient", "phone number", "msisdn"):
		return channel.SendPermanent, "recipient", true
	}
	return 0, "", false
}

// messageText is mNotify's own explanation for logs and send_attempts: the
// "message" field (a string, or an object of validation messages) with the key
// scrubbed and long digit runs, which could be phone numbers, masked.
func (p *Provider) messageText(resp sendResponse) string {
	return p.cleanText(rawText(resp.Message))
}

// detail does the same for a body that may not be JSON at all.
func (p *Provider) detail(raw []byte) string {
	var resp sendResponse
	if json.Unmarshal(raw, &resp) != nil {
		return "no error message"
	}
	if t := p.messageText(resp); t != "" {
		return t
	}
	return "no error message"
}

var digitRun = regexp.MustCompile(`\d{7,}`)

func (p *Provider) cleanText(s string) string {
	s = p.api.scrub(strings.TrimSpace(s))
	s = digitRun.ReplaceAllString(s, "[number]")
	if s == "" {
		return "no error message"
	}
	return truncate(s)
}

// rawText returns a JSON string's value, the compact text of an object or
// array, a number as printed, or "" for null or absent.
func rawText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		return compact.String()
	}
	return ""
}

// noteCredit warns, at most once per lowCreditInterval, that the account is
// nearly out of credits.
func (p *Provider) noteCredit(left int64) {
	if left < 0 || left >= lowCreditWarn {
		return
	}
	now := p.now().UnixNano()
	last := p.lowCreditAt.Load()
	if last != 0 && now-last < int64(lowCreditInterval) {
		return
	}
	if p.lowCreditAt.CompareAndSwap(last, now) {
		p.log.Warn("mNotify SMS credit is low: top up in the mNotify dashboard, sends are rejected at zero", "credit_left", left)
	}
}

// ParseInbound refuses everything: the OpenAPI file documents no inbound-SMS
// webhook, and this system uses mNotify for USSD and MT SMS only.
func (p *Provider) ParseInbound(r *http.Request, body []byte) ([]channel.InboundSMS, error) {
	return nil, fmt.Errorf("%w: mnotify inbound SMS is not used", channel.ErrBadPayload)
}

// ParseStatus refuses everything: mNotify documents no delivery-report
// webhook (only pull calls), so no request can legitimately arrive here.
// Messages stay 'submitted'.
func (p *Provider) ParseStatus(r *http.Request, body []byte) ([]channel.DeliveryReceipt, error) {
	return nil, fmt.Errorf("%w: mnotify documents no delivery-report webhook", channel.ErrBadPayload)
}
