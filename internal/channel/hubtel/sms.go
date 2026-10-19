package hubtel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"smsaighana/internal/channel"
)

const (
	sendPath        = "/v1/messages/send"
	maxResponseBody = 64 << 10
	maxMessageIDLen = 128
)

// sendRequest is the JSON body of POST /v1/messages/send. From, To and
// Content are mandatory; RegisteredDelivery asks for a delivery report, and
// ClientReference is echoed in delivery-report notifications.
type sendRequest struct {
	From               string `json:"From"`
	To                 string `json:"To"`
	Content            string `json:"Content"`
	RegisteredDelivery bool   `json:"RegisteredDelivery"`
	ClientReference    string `json:"ClientReference,omitempty"`
}

// sendResponse is the success body. Hubtel's examples show Status, MessageId,
// Rate, NetworkId and ClientReference at the top level; Data is read as well
// so a wrapped body still yields the id (a missing id makes the send
// uncertain, which stops automatic retries).
type sendResponse struct {
	MessageID flexString      `json:"messageId"`
	Data      json.RawMessage `json:"data"`
}

// Send submits one MT SMS. Errors are classified for the dispatcher:
// Retryable only when Hubtel certainly did not accept the message, Uncertain
// when it may have, Permanent for rejections.
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
		From: msg.SenderID, To: to, Content: msg.Body, RegisteredDelivery: true, ClientReference: msg.ClientRef,
	})
	if err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+sendPath, bytes.NewReader(body))
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
		return channel.SendResult{}, &channel.SendError{Kind: kind, Err: fmt.Errorf("hubtel: %w", err)}
	}
	switch {
	case status >= 200 && status < 300:
		if err != nil {
			return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d but reading the response failed: %v", status, err)
		}
		return accepted(status, raw)
	case status >= 500:
		// A server error does not prove nothing was queued.
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d: %s", status, detail(raw))
	case notAcceptedStatus(status):
		return channel.SendResult{}, sendErr(channel.SendRetryable, "HTTP %d: %s", status, detail(raw))
	default:
		return channel.SendResult{}, sendErr(channel.SendPermanent, "HTTP %d: %s", status, detail(raw))
	}
}

// notAcceptedStatus lists responses that certainly mean "not accepted" for
// reasons outside the message itself (credentials, balance, permissions, rate
// limits, a wrong base URL), so a later retry may succeed.
func notAcceptedStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 300 && status < 400
}

func accepted(status int, raw []byte) (channel.SendResult, error) {
	var sr sendResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with an unreadable body", status)
	}
	id := string(sr.MessageID)
	if id == "" {
		id = nestedMessageID(sr.Data)
	}
	if id == "" || len(id) > maxMessageIDLen {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d accepted without a usable message id", status)
	}
	return channel.SendResult{ProviderMessageID: id}, nil
}

// nestedMessageID reads the id from a Data object or the first element of a
// Data list.
func nestedMessageID(data json.RawMessage) string {
	var one struct {
		MessageID flexString `json:"messageId"`
	}
	if json.Unmarshal(data, &one) == nil && one.MessageID != "" {
		return string(one.MessageID)
	}
	var many []struct {
		MessageID flexString `json:"messageId"`
	}
	if json.Unmarshal(data, &many) == nil && len(many) > 0 {
		return string(many[0].MessageID)
	}
	return ""
}

// do performs an authenticated API call. written reports whether the request
// was completely sent before any transport error, which decides between
// retryable and uncertain failures. status is 0 without a response.
func (p *Provider) do(req *http.Request) (status int, body []byte, written bool, err error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	req.SetBasicAuth(p.opt.ClientID, p.opt.ClientSecret)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, wrote.Load(), err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, body, true, err
}

// ParseInbound refuses everything: Track A uses Hubtel for USSD and MT SMS
// only, so nothing legitimate arrives on the inbound route.
func (p *Provider) ParseInbound(r *http.Request, body []byte) ([]channel.InboundSMS, error) {
	return nil, fmt.Errorf("%w: hubtel inbound SMS is not used", channel.ErrBadPayload)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sendErr(kind channel.SendErrorKind, format string, args ...any) error {
	return &channel.SendError{Kind: kind, Err: fmt.Errorf("hubtel: "+format, args...)}
}

// detail extracts Hubtel's error text for logs and send_attempts. It never
// echoes the request, so no phone number or message text leaks. The error
// body format is not documented, so any of the usual keys is accepted.
func detail(raw []byte) string {
	var e struct {
		Message  string `json:"message"`
		Detail   string `json:"detail"`
		Error    string `json:"error"`
		Response string `json:"response"`
	}
	if json.Unmarshal(raw, &e) == nil {
		for _, s := range []string{e.Message, e.Detail, e.Error, e.Response} {
			if s != "" {
				return truncate(s)
			}
		}
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
