package mtn

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"smsaighana/internal/channel"
)

// lookupResponse is the 200 body of the SMS V2 delivery-status lookup
// (DeliveryStatusResponse): the request id, our clientCorrelator, and one
// status per receiver.
type lookupResponse struct {
	StatusCode flexString `json:"statusCode"`
	Data       struct {
		RequestID        flexString `json:"requestId"`
		ClientCorrelator flexString `json:"clientCorrelator"`
		DeliveryStatus   []struct {
			ReceiverAddress flexString `json:"receiverAddress"`
			Status          flexString `json:"status"`
		} `json:"deliveryStatus"`
	} `json:"data"`
}

// Lookup asks MTN for the delivery status of one request: GET
// /v1/messages/sms/outbound/{senderAddress}/{requestId}/deliveryStatus. It
// returns the request id, our client reference as MTN echoes it, and the
// receipt. Any answer that is not a readable status for the request is an
// error, because "MTN does not know this id" and "this id is not the kind the
// path takes" cannot be told apart.
func (p *Provider) Lookup(ctx context.Context, requestID string) (receipt channel.DeliveryReceipt, clientRef string, err error) {
	if p.opt.SenderID == "" {
		return receipt, "", errorf("status lookup needs the sender ID (SMS_OUTBOUND_SENDER_ID)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.api.url(smsStatusPath(p.opt.SenderID, requestID)), nil)
	if err != nil {
		return receipt, "", err
	}
	req.Header.Set("transactionId", newTransactionID())
	status, raw, _, err := p.api.do(req, smsAuth(p.opt))
	if err != nil {
		return receipt, "", err
	}
	if status < 200 || status >= 300 {
		return receipt, "", errorf("status lookup: HTTP %d: %s", status, detail(raw))
	}
	var lr lookupResponse
	if json.Unmarshal(raw, &lr) != nil || len(lr.Data.DeliveryStatus) == 0 {
		return receipt, "", errorf("status lookup: HTTP %d without a delivery status", status)
	}
	if c := string(lr.StatusCode); c != "" && c != "0000" {
		return receipt, "", errorf("status lookup: canonical status %q", truncate(c))
	}
	if strings.TrimSpace(string(lr.Data.DeliveryStatus[0].Status)) == "" {
		return receipt, "", errorf("status lookup: empty delivery status")
	}
	st, code := mapStatus(string(lr.Data.DeliveryStatus[0].Status))
	id := strings.TrimSpace(string(lr.Data.RequestID))
	if id == "" || len(id) > maxMessageIDLen {
		id = requestID
	}
	receipt = channel.DeliveryReceipt{Provider: Name, ProviderMessageID: id, Status: st, ErrorCode: code, At: p.now()}
	return receipt, strings.TrimSpace(string(lr.Data.ClientCorrelator)), nil
}

// Sender returns the provider as the channel.Sender to register. With
// Options.LookupByClientRef it is wrapped so that it also implements
// channel.StatusQuerier; without, it does not (see the package comment).
func (p *Provider) Sender() channel.Sender {
	if p.opt.LookupByClientRef {
		return lookupSender{p}
	}
	return p
}

// lookupSender adds channel.StatusQuerier to the provider.
type lookupSender struct{ *Provider }

// QueryStatus confirms a send whose outcome is unknown, by the MTN request id
// when the attempt has one and by our own client reference otherwise (the
// assumption MTN_SMS_LOOKUP_BY_CLIENT_REF turns on). It only ever answers
// "found" or an error, never "not found": a missing answer would make the
// reconciler resend a message that may already be delivered and billed, and
// with an undocumented lookup key silence proves nothing. Found requires MTN to
// echo our client reference.
func (l lookupSender) QueryStatus(ctx context.Context, clientRef, providerMessageID string) (channel.DeliveryReceipt, bool, error) {
	id := providerMessageID
	if id == "" {
		id = clientRef
	}
	if id == "" {
		return channel.DeliveryReceipt{}, false, errorf("status lookup: no request id or client reference")
	}
	receipt, echoed, err := l.Provider.Lookup(ctx, id)
	if err != nil {
		return channel.DeliveryReceipt{}, false, err
	}
	if clientRef == "" || echoed != clientRef {
		return channel.DeliveryReceipt{}, false, errorf("status lookup: the answer does not carry our client reference")
	}
	return receipt, true, nil
}
