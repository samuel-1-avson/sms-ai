package mtn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Registrar registers and removes the callback URLs MTN posts to. It is used
// by `admin mtn-subscribe` and `admin mtn-unsubscribe`, never by the serving
// path, and its tests run against fake servers only.
//
// MTN's Swagger files expose no operation that lists subscriptions, so the
// only record of a subscription id is the response to the call that created
// it. The admin tool prints it so the operator can keep it.
type Registrar struct {
	api *API
	opt Options
}

func NewRegistrar(opt Options) *Registrar { return &Registrar{api: newAPI(opt), opt: opt} }

// Tokens returns the OAuth token source, so the admin tool can check the
// credentials without sending anything.
func (r *Registrar) Tokens() *TokenSource { return r.api.tokens }

// USSDSubscription is the SubscriptionRequest of MTN's USSD Swagger: the
// service code to serve, the URL MTN's gateway posts inbound messages to, and
// a name for the destination system.
type USSDSubscription struct {
	ServiceCode  string `json:"serviceCode"`
	CallbackURL  string `json:"callbackUrl"`
	TargetSystem string `json:"targetSystem"`
}

// SMSSubscription is the DeliveryReceiptSubscription of MTN's SMS V2 Swagger:
// the URL MTN posts delivery notifications for messages sent from SenderAddress
// to, and a name for the destination system. SenderAddress is a path parameter.
type SMSSubscription struct {
	SenderAddress string `json:"-"`
	NotifyURL     string `json:"notifyUrl"`
	TargetSystem  string `json:"targetSystem"`
}

// SubscribeUSSD registers the callback for a USSD service code (POST
// /v1/messages/ussd/subscription, x-api-key) and returns data.subscriptionId.
func (r *Registrar) SubscribeUSSD(ctx context.Context, s USSDSubscription) (string, error) {
	return r.subscribe(ctx, ussdSubscriptionAPI, ussdAuth(r.opt), s, nil)
}

// UnsubscribeUSSD removes it again (DELETE /v1/messages/ussd/subscription/{id}).
func (r *Registrar) UnsubscribeUSSD(ctx context.Context, id string) error {
	return r.unsubscribe(ctx, ussdSubscriptionAPI+"/"+url.PathEscape(id), ussdAuth(r.opt), nil)
}

// SubscribeSMS registers the delivery-notification callback of a sender
// address (POST /v1/messages/sms/outbound/{senderAddress}/subscription, OAuth
// bearer token, headers transactionId and x-country-code) and returns data.id.
func (r *Registrar) SubscribeSMS(ctx context.Context, s SMSSubscription) (string, error) {
	return r.subscribe(ctx, smsSubscriptionPath(s.SenderAddress), smsAuth(r.opt), s, r.smsHeaders())
}

// UnsubscribeSMS removes it again (DELETE .../subscription/{id}).
func (r *Registrar) UnsubscribeSMS(ctx context.Context, senderAddress, id string) error {
	return r.unsubscribe(ctx, smsSubscriptionPath(senderAddress)+"/"+url.PathEscape(id), smsAuth(r.opt), r.smsHeaders())
}

// smsHeaders are the headers the SMS subscription calls require besides the
// credentials.
func (r *Registrar) smsHeaders() map[string]string {
	cc := r.opt.CountryCode
	if cc == "" {
		cc = DefaultCountryCode
	}
	return map[string]string{"x-country-code": cc}
}

// subscriptionResponse covers both products: the USSD one answers
// data.subscriptionId and the SMS V2 one data.id.
type subscriptionResponse struct {
	StatusCode flexString `json:"statusCode"`
	Data       struct {
		SubscriptionID flexString `json:"subscriptionId"`
		ID             flexString `json:"id"`
	} `json:"data"`
}

func (r *Registrar) subscribe(ctx context.Context, path string, auth authKind, body any, headers map[string]string) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.api.url(path), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("transactionId", newTransactionID())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	status, resp, _, err := r.api.do(req, auth)
	if err != nil && status == 0 {
		return "", fmt.Errorf("registering callback: %w", err)
	}
	// The USSD Swagger documents 201 for the creating call and the SMS one 200;
	// any 2xx is accepted.
	if status < 200 || status >= 300 {
		return "", errorf("registering callback: HTTP %d: %s", status, detail(resp))
	}
	var sr subscriptionResponse
	id := ""
	if json.Unmarshal(resp, &sr) == nil {
		id = string(sr.Data.SubscriptionID)
		if id == "" {
			id = string(sr.Data.ID)
		}
	}
	if id == "" {
		return "", errorf("registering callback: HTTP %d without a subscription id: %s", status, detail(resp))
	}
	if c := string(sr.StatusCode); c != "" && c != "0000" {
		return "", errorf("registering callback: canonical status %q: %s", truncate(c), detail(resp))
	}
	return id, nil
}

func (r *Registrar) unsubscribe(ctx context.Context, path string, auth authKind, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, r.api.url(path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("transactionId", newTransactionID())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	status, resp, _, err := r.api.do(req, auth)
	if err != nil && status == 0 {
		return fmt.Errorf("removing callback: %w", err)
	}
	if status < 200 || status >= 300 {
		return errorf("removing callback: HTTP %d: %s", status, detail(resp))
	}
	return nil
}

// newTransactionID is the client-generated transactionId header MTN's
// platform uses to trace a request; the SMS subscription calls require it.
func newTransactionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "smsai-" + hex.EncodeToString(b)
}
