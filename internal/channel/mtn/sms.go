package mtn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"smsaighana/internal/channel"
)

// sendRequest is the JSON body of POST /v1/messages/sms/outbound
// (OutboundSMSMessageRequest in MTN's SMS V2 Swagger). senderAddress,
// message and clientCorrelator are required; receiverAddress is an array.
type sendRequest struct {
	SenderAddress    string   `json:"senderAddress"`
	ReceiverAddress  []string `json:"receiverAddress"`
	Message          string   `json:"message"`
	ClientCorrelator string   `json:"clientCorrelator"`
}

// sendResponse is the 200 body (OutboundSMSMessageResponse): MTN's canonical
// status code ("0000" is success) and data with the request id MTN assigned
// (requestId) next to our clientCorrelator. The Swagger gives no example, so
// the success code is inferred from the other MADAPI files.
type sendResponse struct {
	StatusCode    flexString `json:"statusCode"`
	StatusMessage string     `json:"statusMessage"`
	Data          struct {
		RequestID flexString `json:"requestId"`
	} `json:"data"`
}

// Send submits one MT SMS. Errors are classified for the dispatcher:
// Retryable only when MTN certainly did not accept the message, Uncertain
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
	case msg.ClientRef == "" || len(msg.ClientRef) > MaxClientRefLen:
		// The reference is what ties a delivery notification back to the message
		// when MTN echoes it, so a send without one could not be tracked.
		return channel.SendResult{}, sendErr(channel.SendPermanent, "client reference must be 1-%d characters", MaxClientRefLen)
	}
	body, err := json.Marshal(sendRequest{
		SenderAddress: msg.SenderID, ReceiverAddress: []string{to}, Message: msg.Body, ClientCorrelator: msg.ClientRef,
	})
	if err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.api.url(smsSendPath), bytes.NewReader(body))
	if err != nil {
		return channel.SendResult{}, &channel.SendError{Kind: channel.SendPermanent, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	// MTN traces requests by a client-generated transactionId header; our
	// message id carries no personal data.
	req.Header.Set("transactionId", msg.ClientRef)

	status, raw, written, err := p.api.do(req, smsAuth(p.opt))
	if status == 0 {
		// No response. A request that never fully left (including a failed
		// token request) cannot have been accepted; one that did may have been.
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
		return accepted(status, raw, msg.ClientRef)
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
// reasons outside the message itself (credentials, permissions, balance, rate
// limits, a wrong base URL, a proxy requiring authentication), so a later
// retry may succeed. 407 is in the v3 Swagger's list of responses.
func notAcceptedStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusProxyAuthRequired, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 300 && status < 400
}

// accepted checks a 2xx body. The message id is the request id MTN assigned,
// which the delivery-status lookup takes and a delivery notification may carry;
// notifications are matched on it and on our client reference alike (dlr.go).
// Without a usable request id the client reference stands in.
func accepted(status int, raw []byte, clientRef string) (channel.SendResult, error) {
	var sr sendResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with an unreadable body", status)
	}
	// 0000 is MTN's canonical "success"; a body without a code but with a
	// request id is taken as success too. Anything else on a 2xx is a
	// contradiction worth a human look, not a retry.
	code := string(sr.StatusCode)
	id := strings.TrimSpace(string(sr.Data.RequestID))
	if code != "0000" && (code != "" || id == "") {
		return channel.SendResult{}, sendErr(channel.SendUncertain, "HTTP %d with canonical status %q: %s",
			status, truncate(code), truncate(sr.StatusMessage))
	}
	if id == "" || len(id) > maxMessageIDLen {
		id = clientRef
	}
	return channel.SendResult{ProviderMessageID: id}, nil
}

// ParseInbound refuses everything: Track A uses MTN for USSD and MT SMS only,
// and the SMS V2 API has no mobile-originating callback to register.
func (p *Provider) ParseInbound(r *http.Request, body []byte) ([]channel.InboundSMS, error) {
	return nil, fmt.Errorf("%w: mtn inbound SMS is not used", channel.ErrBadPayload)
}
