package hubtel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"smsaighana/internal/channel"
)

// exitInput is the engine's "0.Exit" entry. Hubtel's release and timeout
// requests end a session; the channel contract has no field for that, so they
// are delivered as the user choosing Exit, which ends the engine's session
// without booking a question or spending quota.
const exitInput = "0"

// ussdLabel is the title Hubtel shows on web and mobile channels. Programmable
// Services requires one even though a handset only displays the message.
const ussdLabel = "SMS AI Ghana"

// USSDPayload is the body Hubtel POSTs to the Service Interaction URL.
// Hubtel's documentation uses both camelCase and PascalCase keys; encoding/json
// matches either. Sequence and ClientState are not decoded: the adapter does
// not use them.
type USSDPayload struct {
	Type        string     `json:"type"`
	Platform    string     `json:"platform"`
	Message     flexString `json:"message"`
	Mobile      flexString `json:"mobile"`
	Operator    string     `json:"operator"`
	ServiceCode flexString `json:"serviceCode"`
	SessionID   flexString `json:"sessionId"`
}

// USSDResponse is the reply Hubtel expects: type Response keeps the session
// open, Release ends it. label, dataType and fieldType are marked mandatory
// in Hubtel's table; item and data are null for plain text screens.
type USSDResponse struct {
	Type        string  `json:"type"`
	Message     string  `json:"message"`
	SessionID   string  `json:"SessionId"`
	ClientState *string `json:"clientState"`
	Label       string  `json:"label"`
	DataType    string  `json:"dataType"`
	FieldType   string  `json:"fieldType"`
	FieldName   string  `json:"fieldName"`
	Item        any     `json:"item"`
	Data        any     `json:"data"`
}

func (p *Provider) ParseSession(r *http.Request, body []byte) (channel.USSDRequest, error) {
	if err := p.authenticate(r, p.opt.USSDTokens, p.opt.USSDAllowedIPs, "HUBTEL_USSD_TOKEN"); err != nil {
		return channel.USSDRequest{}, err
	}
	var u USSDPayload
	if err := json.Unmarshal(body, &u); err != nil || u.SessionID == "" || u.Mobile == "" {
		return channel.USSDRequest{}, fmt.Errorf("%w: need sessionId and mobile", channel.ErrBadPayload)
	}
	if u.Platform != "" && !strings.EqualFold(u.Platform, "ussd") {
		return channel.USSDRequest{}, fmt.Errorf("%w: only the USSD platform is served (got %q)", channel.ErrBadPayload, truncate(u.Platform))
	}

	req := channel.USSDRequest{
		Provider: Name,
		// The reply only echoes the session id, so it needs no adapter state.
		SessionID: string(u.SessionID),
		FromE164:  string(u.Mobile), // as received, e.g. 233208074263
		Network:   strings.ToLower(u.Operator),
		// ServiceCode stays empty: Hubtel's sample requests carry the dialled
		// shortcode, but its SDK sample carries a service GUID, so the
		// engine's configured USSD_SERVICE_CODE keeps every step on the same
		// conversation.
	}
	switch strings.ToLower(strings.TrimSpace(u.Type)) {
	case "initiation":
		// message is empty in Hubtel's sample request and the dialled code
		// in its table and SDK sample; neither is a menu entry.
		req.NewSession = true
	case "response":
		// message is the user's latest entry only: "1", then "2", then "1"
		// in Hubtel's sample session, never a joined "1*2*1" history.
		req.Input = string(u.Message)
	case "release", "timeout":
		// The session is over on Hubtel's side and the reply is not shown to
		// the user (Hubtel's SDK calls a timeout request ignorable).
		req.Input = exitInput
	default:
		return channel.USSDRequest{}, fmt.Errorf("%w: unsupported request type %q", channel.ErrBadPayload, truncate(u.Type))
	}
	return req, nil
}

func (p *Provider) WriteReply(w http.ResponseWriter, req channel.USSDRequest, reply channel.USSDReply) error {
	resp := USSDResponse{
		Type: "Response", Message: reply.Message, SessionID: req.SessionID,
		Label: ussdLabel, DataType: "input", FieldType: "text",
	}
	if reply.End {
		resp.Type, resp.DataType = "Release", "display"
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}
