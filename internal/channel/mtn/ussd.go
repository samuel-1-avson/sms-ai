package mtn

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"smsaighana/internal/channel"
)

// USSD messageType values, from the InboundRequest of MTN's USSD Swagger:
// "0-Begin|1-Continue|2-End|3-Notification|4-Cancel|5-Timeout".
const (
	msgBegin        = "0"
	msgContinue     = "1"
	msgEnd          = "2"
	msgNotification = "3"
	msgCancel       = "4"
	msgTimeout      = "5"
)

// exitInput is the engine's "0.Exit" entry. MTN's End, Cancel and Timeout
// messages end a session; the channel contract has no field for that, so they
// are delivered as the user choosing Exit, which ends the engine's session
// without booking a question or spending quota.
const exitInput = "0"

// replyPath is the self link in replies; MTN's schema requires one.
const replyPath = "/webhooks/ussd/mtn/session"

// InboundRequest is the body MTN's USSD gateway posts to the registered
// callback URL. cellId, language and imsi exist in the schema and are not
// decoded: the adapter does not use them. messageType is a string in the
// schema and an integer in replies, so both forms are accepted.
type InboundRequest struct {
	SessionID   flexString `json:"sessionId"`
	MessageType flexString `json:"messageType"`
	MSISDN      flexString `json:"msisdn"`
	ServiceCode flexString `json:"serviceCode"`
	USSDString  flexString `json:"ussdString"`
}

// InboundResponse is the reply the schema describes for the callback:
// statusCode "0000", an envelope and data. The schema marks data.outboundResponse
// as required but defines data.inboundResponse; the documented property is
// sent (docs/PROVIDER_VALIDATION.md lists the discrepancy).
type InboundResponse struct {
	StatusCode    string       `json:"statusCode"`
	StatusMessage string       `json:"statusMessage"`
	TransactionID string       `json:"transactionId"`
	Data          InboundData  `json:"data"`
	Link          ResponseLink `json:"_link"`
}

type InboundData struct {
	InboundResponse   string `json:"inboundResponse"`
	UserInputRequired bool   `json:"userInputRequired"`
	MessageType       int    `json:"messageType"`
	ServiceCode       string `json:"serviceCode,omitempty"`
	MSISDN            string `json:"msisdn"`
}

type ResponseLink struct {
	Self struct {
		Href string `json:"href"`
	} `json:"self"`
}

func (p *Provider) ParseSession(r *http.Request, body []byte) (channel.USSDRequest, error) {
	if err := p.authenticate(r, p.opt.USSDTokens, p.opt.USSDAllowedIPs, "MTN_USSD_TOKEN"); err != nil {
		return channel.USSDRequest{}, err
	}
	var u InboundRequest
	if err := json.Unmarshal(body, &u); err != nil || u.SessionID == "" || u.MSISDN == "" || u.MessageType == "" {
		return channel.USSDRequest{}, fmt.Errorf("%w: need sessionId, msisdn and messageType", channel.ErrBadPayload)
	}

	req := channel.USSDRequest{
		Provider:  Name,
		SessionID: string(u.SessionID),
		// As received, e.g. 233241234567 without a "+"; the engine normalizes
		// it, and the reply echoes it back unchanged.
		FromE164: string(u.MSISDN),
		Network:  Name,
		// ServiceCode stays empty so the engine's configured USSD_SERVICE_CODE
		// keys the conversation however MTN spells the code per message.
	}
	switch string(u.MessageType) {
	case msgBegin:
		// ussdString holds the dialled string (or nothing); it is not a menu
		// entry.
		req.NewSession = true
	case msgContinue:
		req.Input = p.latestEntry(string(u.USSDString))
	case msgEnd, msgCancel, msgTimeout:
		// The session is over on MTN's side and the reply is not shown to the
		// user.
		req.Input = exitInput
	case msgNotification:
		return channel.USSDRequest{}, fmt.Errorf("%w: USSD notifications are not served", channel.ErrBadPayload)
	default:
		return channel.USSDRequest{}, fmt.Errorf("%w: unsupported messageType %q", channel.ErrBadPayload, truncate(string(u.MessageType)))
	}
	return req, nil
}

// latestEntry returns the user's latest entry. MTN does not say whether
// ussdString is that entry or the accumulated "1*2*text" string; the default
// is the former, and USSDInputIsHistory selects the latter.
func (p *Provider) latestEntry(s string) string {
	if p.opt.USSDInputIsHistory {
		if i := strings.LastIndexByte(s, '*'); i >= 0 {
			return s[i+1:]
		}
	}
	return s
}

// WriteReply answers in the HTTP response body, as MTN's gateway expects of a
// callback. CON becomes messageType 1 (Continue) with userInputRequired, END
// becomes messageType 2 (End).
func (p *Provider) WriteReply(w http.ResponseWriter, req channel.USSDRequest, reply channel.USSDReply) error {
	resp := InboundResponse{
		StatusCode: "0000", StatusMessage: "Successful",
		Data: InboundData{
			InboundResponse: reply.Message, UserInputRequired: !reply.End, MessageType: 1,
			ServiceCode: p.opt.USSDServiceCode, MSISDN: req.FromE164,
		},
	}
	resp.Link.Self.Href = replyPath
	if reply.End {
		resp.Data.MessageType = 2
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}
