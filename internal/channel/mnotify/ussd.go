package mnotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"smsaighana/internal/channel"
)

// mNotify offers two USSD products over the same kind of HTTPS callback, both
// documented in the OpenAPI file's USSD tag:
//
// Shared (a common short code; the first request carries the dialled string):
//
//	POST  {"msisdn":"233541509394","sequenceID":"6789940010","data":"*899*86#","timestamp":"2025-09-01T11:22:33Z"}
//	reply {"msisdn":..., "sequenceID":..., "message":"...\r\n...", "timestamp":..., "continueFlag":0|1}
//
// Dedicated (our own short code):
//
//	POST  {"shortCode":"*123#","msIsdn":"233541509394","text":"1","sessId":"ABC123456","ussdGwId":"MTN","language":"en"}
//	reply {"responseExitCode":200, "shouldClose":false, "ussdMenu":"...\r\n...", "responseMessage":"..."}
//
// The payload shape says which mode a request is, so one route serves both
// (MNOTIFY_USSD_MODE can pin it). Replies must use \r\n for line breaks; the
// engine writes \n.
//
// Shared mode has no session id: the session is keyed by the msisdn. The file
// does not say whether sequenceID identifies one request or one whole session;
// either way it is echoed per request and never used as the key.
//
// Replies echo request values that channel.USSDRequest has no field for, so
// ParseSession passes them to WriteReply in USSDRequest.Network, which nothing
// else reads. The value is "shared|<sequenceID>" or "dedicated|<ussdGwId>".
const (
	ctxShared    = "shared"
	ctxDedicated = "dedicated"
)

// endedTTL bounds how long a finished shared session is remembered. It exceeds
// the engine's USSD session TTL (180 s by default), which is how long the
// engine would otherwise keep serving the finished session's stored state.
const (
	endedTTL = 5 * time.Minute
	endedMax = 10000
)

// sharedResponse is the reply to a shared-USSD callback. continueFlag is 0 to
// continue and 1 to end.
type sharedResponse struct {
	MSISDN       string `json:"msisdn"`
	SequenceID   string `json:"sequenceID"`
	Message      string `json:"message"`
	Timestamp    string `json:"timestamp"`
	ContinueFlag int    `json:"continueFlag"`
}

// dedicatedResponse is the reply to a dedicated-USSD callback.
type dedicatedResponse struct {
	ResponseExitCode int    `json:"responseExitCode"`
	ShouldClose      bool   `json:"shouldClose"`
	USSDMenu         string `json:"ussdMenu"`
	ResponseMessage  string `json:"responseMessage"`
}

// fields decodes a JSON object into lower-cased keys. mNotify's two products
// spell the phone number "msisdn" and "msIsdn"; Go's decoder would treat those
// as the same field, so the adapter does the matching itself.
type fields map[string]json.RawMessage

func decodeFields(body []byte) (fields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, errors.New("not a JSON object")
	}
	f := make(fields, len(raw))
	for k, v := range raw {
		f[strings.ToLower(k)] = v
	}
	return f, nil
}

func (f fields) has(key string) bool { _, ok := f[key]; return ok }

func (f fields) str(key string) string {
	var s flexString
	if raw, ok := f[key]; ok {
		_ = s.UnmarshalJSON(raw)
	}
	return strings.TrimSpace(string(s))
}

func (p *Provider) ParseSession(r *http.Request, body []byte) (channel.USSDRequest, error) {
	if err := p.authenticate(r); err != nil {
		return channel.USSDRequest{}, err
	}
	f, err := decodeFields(body)
	if err != nil {
		return channel.USSDRequest{}, fmt.Errorf("%w: %v", channel.ErrBadPayload, err)
	}
	dedicated := f.has("sessid") || f.has("shortcode") || f.has("ussdgwid")
	shared := f.has("sequenceid") || f.has("data")
	switch {
	case dedicated && shared:
		return channel.USSDRequest{}, fmt.Errorf("%w: payload mixes shared and dedicated USSD fields", channel.ErrBadPayload)
	case dedicated:
		if p.opt.USSDMode == ModeShared {
			return channel.USSDRequest{}, fmt.Errorf("%w: a dedicated-USSD payload arrived but MNOTIFY_USSD_MODE=shared", channel.ErrBadPayload)
		}
		return p.parseDedicated(f)
	case shared:
		if p.opt.USSDMode == ModeDedicated {
			return channel.USSDRequest{}, fmt.Errorf("%w: a shared-USSD payload arrived but MNOTIFY_USSD_MODE=dedicated", channel.ErrBadPayload)
		}
		return p.parseShared(f)
	}
	return channel.USSDRequest{}, fmt.Errorf("%w: need msisdn, sequenceID and data (shared) or msIsdn, sessId and text (dedicated)", channel.ErrBadPayload)
}

func (p *Provider) parseShared(f fields) (channel.USSDRequest, error) {
	msisdn, seq := f.str("msisdn"), f.str("sequenceid")
	if msisdn == "" || seq == "" || !f.has("data") {
		return channel.USSDRequest{}, fmt.Errorf("%w: shared USSD needs msisdn, sequenceID and data", channel.ErrBadPayload)
	}
	data := f.str("data")
	key := sessionKey(msisdn)
	// A dialled string starts a session. So does any request that follows a
	// reply we ended: the user's handset closed that session, whatever the
	// request looks like.
	wasEnded := p.ended.take(key, p.now())
	req := channel.USSDRequest{
		Provider:  Name,
		SessionID: key,
		// As received, e.g. 233541509394 without a "+"; the engine normalizes it
		// and the reply echoes it back unchanged.
		FromE164:   msisdn,
		NewSession: isDialString(data) || wasEnded,
		Network:    ctxShared + "|" + seq,
		// ServiceCode stays empty so the engine's configured USSD_SERVICE_CODE
		// keys the conversation however the shared code is spelled per request.
	}
	if !req.NewSession {
		req.Input = data
	}
	return req, nil
}

func (p *Provider) parseDedicated(f fields) (channel.USSDRequest, error) {
	msisdn, sess := f.str("msisdn"), f.str("sessid")
	if msisdn == "" || sess == "" || !f.has("text") {
		return channel.USSDRequest{}, fmt.Errorf("%w: dedicated USSD needs msIsdn, sessId and text", channel.ErrBadPayload)
	}
	text := f.str("text")
	req := channel.USSDRequest{
		Provider:  Name,
		SessionID: sess,
		FromE164:  msisdn,
		// The file does not say what the first request's text holds; the dialled
		// code is the likely value, and no menu entry looks like one. A session id
		// the engine has not seen is a new session anyway.
		NewSession: isDialString(text),
		Network:    ctxDedicated + "|" + strings.ToLower(f.str("ussdgwid")),
		// ServiceCode stays empty, as for the shared mode.
	}
	if !req.NewSession {
		req.Input = text
	}
	return req, nil
}

// WriteReply answers in the HTTP response body, in the format of the mode the
// request arrived in.
func (p *Provider) WriteReply(w http.ResponseWriter, req channel.USSDRequest, reply channel.USSDReply) error {
	mode, detail, _ := strings.Cut(req.Network, "|")
	text := crlf(reply.Message)
	var out any
	switch mode {
	case ctxShared:
		key := sessionKey(req.FromE164)
		if reply.End {
			p.ended.mark(key, p.now())
		} else {
			p.ended.clear(key)
		}
		flag := 0
		if reply.End {
			flag = 1
		}
		out = sharedResponse{
			MSISDN: req.FromE164, SequenceID: detail, Message: text,
			Timestamp: p.now().UTC().Format(time.RFC3339), ContinueFlag: flag,
		}
	case ctxDedicated:
		out = dedicatedResponse{ResponseExitCode: http.StatusOK, ShouldClose: reply.End, USSDMenu: text, ResponseMessage: "OK"}
	default:
		return errors.New("mnotify: the USSD request carries no reply context (not produced by ParseSession)")
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// crlf converts every line break to \r\n, which mNotify requires in USSD menus.
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// isDialString reports whether s looks like a dialled USSD string such as
// *899*86# rather than something the user typed into a menu: it starts with
// "*" and holds no whitespace.
func isDialString(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "*") && !strings.ContainsAny(s, " \t\r\n")
}

// sessionKey keys a shared-mode session by phone number, digits only, so
// "+233541509394" and "233541509394" are one session.
func sessionKey(msisdn string) string {
	var b strings.Builder
	for _, r := range msisdn {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// endedSessions remembers which shared-mode sessions our last reply ended. It
// lives in this process only: with several API replicas a request after an
// ended session can reach one that did not end it, and then only a dialled
// string starts a new session (the engine keeps an ended session's stored state
// for up to USSD_SESSION_TTL_SECONDS).
type endedSessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newEndedSessions() *endedSessions { return &endedSessions{m: map[string]time.Time{}} }

func (e *endedSessions) mark(key string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.m) >= endedMax {
		for k, at := range e.m {
			if now.Sub(at) > endedTTL {
				delete(e.m, k)
			}
		}
		if len(e.m) >= endedMax {
			e.m = map[string]time.Time{} // forget everything: dial strings still start sessions
		}
	}
	e.m[key] = now
}

// take reports whether the session ended recently and forgets it.
func (e *endedSessions) take(key string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.m[key]
	delete(e.m, key)
	return ok && now.Sub(at) <= endedTTL
}

func (e *endedSessions) clear(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, key)
}
