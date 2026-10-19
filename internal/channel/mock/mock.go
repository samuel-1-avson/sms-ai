// Package mock is a signed, provider-shaped SMS + USSD gateway for local
// development, fixtures and tests. It exercises the same raw-body HMAC
// verification path a real aggregator adapter must implement.
//
// Webhook authentication: header X-Webhook-Timestamp (unix seconds) and
// X-Webhook-Signature = "sha256=" + hex(HMAC-SHA256(secret, ts + "." + body)).
package mock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"smsaighana/internal/channel"
	"smsaighana/internal/security"
)

const (
	Name            = "mock"
	HeaderSignature = "X-Webhook-Signature"
	HeaderTimestamp = "X-Webhook-Timestamp"
	tolerance       = 5 * time.Minute
)

// InboundPayload is the mock inbound SMS webhook body.
type InboundPayload struct {
	MessageID  string `json:"message_id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Text       string `json:"text"`
	ReceivedAt string `json:"received_at,omitempty"`
}

// StatusPayload is the mock delivery receipt body.
type StatusPayload struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

// USSDPayload is the mock USSD session callback body.
type USSDPayload struct {
	SessionID   string `json:"session_id"`
	ServiceCode string `json:"service_code"`
	Phone       string `json:"phone"`
	Input       string `json:"input"`
	NewSession  bool   `json:"new_session"`
	Network     string `json:"network,omitempty"`
}

// USSDResponse is what the mock gateway expects back.
type USSDResponse struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
	Action    string `json:"action"` // CON | END
}

// SentMessage is an MT SMS captured by the mock sender.
type SentMessage struct {
	channel.OutboundSMS
	ProviderMessageID string
	At                time.Time
}

type Provider struct {
	secret string
	now    func() time.Time
	log    *slog.Logger

	dlrURL   string
	dlrDelay time.Duration
	client   *http.Client

	mu       sync.Mutex
	sent     []SentMessage
	byRef    map[string]string
	failNext []channel.SendErrorKind
}

func New(secret, dlrCallbackURL string, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		secret: secret, now: time.Now, log: log,
		dlrURL: dlrCallbackURL, dlrDelay: 300 * time.Millisecond,
		client: &http.Client{Timeout: 5 * time.Second},
		byRef:  map[string]string{},
	}
}

func (p *Provider) Name() string { return Name }

func (p *Provider) verify(r *http.Request, body []byte) error {
	err := security.VerifyWebhook(p.secret, r.Header.Get(HeaderSignature), r.Header.Get(HeaderTimestamp), body, p.now(), tolerance)
	if err != nil {
		return fmt.Errorf("%w: %v", channel.ErrBadSignature, err)
	}
	return nil
}

func (p *Provider) ParseInbound(r *http.Request, body []byte) ([]channel.InboundSMS, error) {
	if err := p.verify(r, body); err != nil {
		return nil, err
	}
	var in InboundPayload
	if err := json.Unmarshal(body, &in); err != nil || in.MessageID == "" || in.From == "" {
		return nil, fmt.Errorf("%w: need message_id, from, text", channel.ErrBadPayload)
	}
	at := p.now()
	if in.ReceivedAt != "" {
		if t, err := time.Parse(time.RFC3339, in.ReceivedAt); err == nil {
			at = t
		}
	}
	return []channel.InboundSMS{{
		Provider: Name, ProviderEventID: in.MessageID, ServiceID: in.To,
		FromE164: in.From, Body: in.Text, ReceivedAt: at, Environment: "sandbox",
	}}, nil
}

func (p *Provider) ParseStatus(r *http.Request, body []byte) ([]channel.DeliveryReceipt, error) {
	if err := p.verify(r, body); err != nil {
		return nil, err
	}
	var st StatusPayload
	if err := json.Unmarshal(body, &st); err != nil || st.MessageID == "" || st.Status == "" {
		return nil, fmt.Errorf("%w: need message_id and status", channel.ErrBadPayload)
	}
	return []channel.DeliveryReceipt{{
		Provider: Name, ProviderMessageID: st.MessageID, Status: strings.ToLower(st.Status),
		ErrorCode: st.ErrorCode, At: p.now(),
	}}, nil
}

func (p *Provider) ParseSession(r *http.Request, body []byte) (channel.USSDRequest, error) {
	if err := p.verify(r, body); err != nil {
		return channel.USSDRequest{}, err
	}
	var u USSDPayload
	if err := json.Unmarshal(body, &u); err != nil || u.SessionID == "" || u.Phone == "" {
		return channel.USSDRequest{}, fmt.Errorf("%w: need session_id and phone", channel.ErrBadPayload)
	}
	return channel.USSDRequest{
		Provider: Name, SessionID: u.SessionID, ServiceCode: u.ServiceCode, FromE164: u.Phone,
		Input: u.Input, NewSession: u.NewSession, Network: u.Network,
	}, nil
}

func (p *Provider) WriteReply(w http.ResponseWriter, req channel.USSDRequest, reply channel.USSDReply) error {
	action := "CON"
	if reply.End {
		action = "END"
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(USSDResponse{SessionID: req.SessionID, Message: reply.Message, Action: action})
}

// Send captures the message and, if configured, posts a signed delivery
// receipt back to the API shortly afterwards.
func (p *Provider) Send(ctx context.Context, msg channel.OutboundSMS) (channel.SendResult, error) {
	p.mu.Lock()
	if len(p.failNext) > 0 {
		kind := p.failNext[0]
		p.failNext = p.failNext[1:]
		p.mu.Unlock()
		return channel.SendResult{}, &channel.SendError{Kind: kind, Err: fmt.Errorf("mock injected failure")}
	}
	id := "mock-" + randomHex(8)
	p.sent = append(p.sent, SentMessage{OutboundSMS: msg, ProviderMessageID: id, At: p.now()})
	p.byRef[msg.ClientRef] = id
	p.mu.Unlock()

	if p.dlrURL != "" {
		go p.postReceipt(id)
	}
	return channel.SendResult{ProviderMessageID: id}, nil
}

// QueryStatus lets the dispatcher reconcile uncertain submissions.
func (p *Provider) QueryStatus(ctx context.Context, clientRef, providerMessageID string) (channel.DeliveryReceipt, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.byRef[clientRef]
	if !ok {
		return channel.DeliveryReceipt{}, false, nil
	}
	return channel.DeliveryReceipt{Provider: Name, ProviderMessageID: id, Status: "submitted", At: p.now()}, true, nil
}

// Sent returns a copy of every captured MT SMS (tests).
func (p *Provider) Sent() []SentMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]SentMessage(nil), p.sent...)
}

// FailNext makes the next Send calls fail with the given kinds (tests).
func (p *Provider) FailNext(kinds ...channel.SendErrorKind) {
	p.mu.Lock()
	p.failNext = append(p.failNext, kinds...)
	p.mu.Unlock()
}

// RecordAcceptedWithoutReply simulates a provider that accepted a message
// but whose HTTP response was lost (tests of reconciliation).
func (p *Provider) RecordAcceptedWithoutReply(clientRef string) {
	p.mu.Lock()
	p.byRef[clientRef] = "mock-" + randomHex(8)
	p.mu.Unlock()
}

func (p *Provider) postReceipt(providerMessageID string) {
	time.Sleep(p.dlrDelay)
	body, _ := json.Marshal(StatusPayload{MessageID: providerMessageID, Status: "delivered"})
	req, err := SignedRequest(context.Background(), p.secret, p.dlrURL, body, p.now())
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Warn("mock delivery receipt failed", "err", err)
		return
	}
	resp.Body.Close()
}

// SignedRequest builds a POST with mock webhook signature headers.
func SignedRequest(ctx context.Context, secret, url string, body []byte, now time.Time) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, security.SignWebhook(secret, ts, body))
	return req, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
