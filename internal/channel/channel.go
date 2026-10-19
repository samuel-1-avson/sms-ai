// Package channel defines the provider-neutral contracts between telecom
// gateways and the domain (plan §4.2 "Channel ingress adapters", §9).
// Adapters for eSMS Africa, Africa's Talking, Arkesel or Hubtel implement
// these interfaces; the domain never sees provider-specific payloads.
package channel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// InboundSMS is the normalized inbound event (plan §9).
type InboundSMS struct {
	Provider        string
	ProviderEventID string
	ServiceID       string
	FromE164        string
	Body            string
	ReceivedAt      time.Time
	Environment     string
}

// DeliveryReceipt is a normalized carrier delivery report.
type DeliveryReceipt struct {
	Provider          string
	ProviderMessageID string
	Status            string // submitted | delivered | failed | expired | rejected | unknown
	ErrorCode         string
	At                time.Time
}

// USSDRequest carries one step of an interactive USSD session. Input is the
// user's latest entry only; adapters for gateways that send the whole
// "1*2*text" history must extract the last element.
type USSDRequest struct {
	Provider    string
	SessionID   string
	ServiceCode string
	FromE164    string
	Input       string
	NewSession  bool
	Network     string
}

// USSDReply is rendered by the gateway adapter as CON (continue) or END.
type USSDReply struct {
	Message string
	End     bool
}

type OutboundSMS struct {
	To        string
	SenderID  string
	Body      string
	Encoding  string
	ClientRef string // our idempotency key (message id)
}

type SendResult struct {
	ProviderMessageID string
}

// SendErrorKind tells the dispatcher whether resending is safe.
type SendErrorKind int

const (
	// SendRetryable: the provider definitely did not accept the message.
	SendRetryable SendErrorKind = iota
	// SendPermanent: rejected (bad number, blocked sender); do not retry.
	SendPermanent
	// SendUncertain: timeout after the request left; the message may have
	// been accepted and billed. Reconcile before any retry (plan §5.4).
	SendUncertain
)

type SendError struct {
	Kind SendErrorKind
	Err  error
}

func (e *SendError) Error() string { return fmt.Sprintf("send error (kind %d): %v", e.Kind, e.Err) }
func (e *SendError) Unwrap() error { return e.Err }

// SendErrorKindOf classifies any error returned by Sender.Send.
func SendErrorKindOf(err error) SendErrorKind {
	var se *SendError
	if errors.As(err, &se) {
		return se.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return SendUncertain
	}
	return SendRetryable
}

var (
	ErrBadSignature = errors.New("channel: webhook signature invalid")
	ErrBadPayload   = errors.New("channel: webhook payload invalid")
)

// SMSInbound parses (and authenticates) inbound SMS and delivery webhooks.
type SMSInbound interface {
	Name() string
	ParseInbound(r *http.Request, body []byte) ([]InboundSMS, error)
	ParseStatus(r *http.Request, body []byte) ([]DeliveryReceipt, error)
}

// Sender submits mobile-terminated SMS.
type Sender interface {
	Name() string
	Send(ctx context.Context, msg OutboundSMS) (SendResult, error)
}

// StatusQuerier is implemented by providers with a query-by-ID API, used to
// reconcile submissions whose outcome is unknown.
type StatusQuerier interface {
	QueryStatus(ctx context.Context, clientRef, providerMessageID string) (receipt DeliveryReceipt, found bool, err error)
}

// USSDGateway parses USSD session callbacks and renders replies.
type USSDGateway interface {
	Name() string
	ParseSession(r *http.Request, body []byte) (USSDRequest, error)
	WriteReply(w http.ResponseWriter, req USSDRequest, reply USSDReply) error
}

// Registry holds the configured providers by name.
type Registry struct {
	smsInbound    map[string]SMSInbound
	senders       map[string]Sender
	ussd          map[string]USSDGateway
	primarySender string
}

func NewRegistry(primarySender string) *Registry {
	return &Registry{
		smsInbound:    map[string]SMSInbound{},
		senders:       map[string]Sender{},
		ussd:          map[string]USSDGateway{},
		primarySender: primarySender,
	}
}

func (r *Registry) AddSMSInbound(p SMSInbound) { r.smsInbound[p.Name()] = p }
func (r *Registry) AddSender(s Sender)         { r.senders[s.Name()] = s }
func (r *Registry) AddUSSD(g USSDGateway)      { r.ussd[g.Name()] = g }

func (r *Registry) SMSInbound(name string) (SMSInbound, bool) {
	p, ok := r.smsInbound[name]
	return p, ok
}

func (r *Registry) USSD(name string) (USSDGateway, bool) {
	g, ok := r.ussd[name]
	return g, ok
}

// SenderFor picks the outbound route for a conversation: a two-way SMS
// provider replies through itself; USSD conversations (and providers that
// cannot send) use the primary MT SMS sender.
func (r *Registry) SenderFor(conversationProvider string) (Sender, error) {
	if s, ok := r.senders[conversationProvider]; ok {
		return s, nil
	}
	if s, ok := r.senders[r.primarySender]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("channel: no SMS sender for %q and primary %q is not configured", conversationProvider, r.primarySender)
}
