package mtn

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smsaighana/internal/channel"
)

func TestParseStatusFromFixtures(t *testing.T) {
	p := newTestProvider("")
	p.now = func() time.Time { return time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC) }

	got, err := p.ParseStatus(dlrHTTP(testDLRToken, ""), fixture(t, "mtn_sms_delivery_notification_constructed.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := channel.DeliveryReceipt{
		Provider: Name, ProviderMessageID: testRequestID, Status: "delivered", ErrorCode: "",
		At: p.now(),
	}
	clientWant := want
	clientWant.ProviderMessageID = testMsgID
	if len(got) != 2 || got[0] != want || got[1] != clientWant {
		t.Fatalf("delivered:\n got %+v\nwant %+v", got, want)
	}

	got, err = p.ParseStatus(dlrHTTP(testDLRToken, ""), fixture(t, "mtn_sms_delivery_notification_undelivered_constructed.json"))
	if err != nil {
		t.Fatal(err)
	}
	want = channel.DeliveryReceipt{
		Provider: Name, ProviderMessageID: testRequestID, Status: "failed", ErrorCode: "ABSENT_SUBSCRIBER",
		At: p.now(),
	}
	clientWant = want
	clientWant.ProviderMessageID = testMsgID
	if len(got) != 2 || got[0] != want || got[1] != clientWant {
		t.Fatalf("undelivered:\n got %+v\nwant %+v", got, want)
	}
	got, err = p.ParseStatus(dlrHTTP(testDLRToken, ""), fixture(t, "mtn_sms_delivery_notification_v3_shaped_constructed.json"))
	if err != nil || len(got) != 1 || got[0].ProviderMessageID != "09273682261681" || got[0].Status != "delivered" ||
		!got[0].At.Equal(time.Date(2026, 10, 17, 14, 30, 5, 0, time.UTC)) {
		t.Fatalf("v3-shaped: %+v %v", got, err)
	}
}

func TestParseStatusShapes(t *testing.T) {
	p := newTestProvider("")
	fixed := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return fixed }
	cases := []struct {
		name string
		body string
		want []string // id:status
	}{
		{"minimal object", `{"clientCorrelatorId":"m1","deliveryStatus":"DELIVERED"}`, []string{"m1:delivered"}},
		{"numeric correlator id", `{"clientCorrelatorId":9273682261681,"deliveryStatus":"DELIVERED"}`, []string{"9273682261681:delivered"}},
		{"lower-case status", `{"clientCorrelatorId":"m1","deliveryStatus":"delivered"}`, []string{"m1:delivered"}},
		{"array of objects", `[{"clientCorrelatorId":"m1","deliveryStatus":"DELIVERED"},{"clientCorrelatorId":"m2","deliveryStatus":"EXPIRED"}]`, []string{"m1:delivered", "m2:expired"}},
		{"wrapped in data", `{"data":{"clientCorrelatorId":"m1","deliveryStatus":"UNDELIVERED"}}`, []string{"m1:failed"}},
		{"array wrapped in data", `{"statusCode":"0000","data":[{"clientCorrelatorId":"m1","deliveryStatus":"REJECTED"}]}`, []string{"m1:rejected"}},
		{"unreadable fields are ignored", `{"clientCorrelatorId":"m1","deliveryStatus":"DELIVERED","error":{"code":1},"id":"x","completedDate":true}`, []string{"m1:delivered"}},
		{"one bad entry among good", `[{"deliveryStatus":"DELIVERED"},{"clientCorrelatorId":"m2","deliveryStatus":"DELIVERED"}]`, []string{"m2:delivered"}},
		{"V2 request id only", `{"requestId":"r1","deliveryStatus":[{"receiverAddress":"233541234567","status":"DeliveredToTerminal"}]}`, []string{"r1:delivered"}},
		{"V2 client reference only", `{"clientCorrelator":"m1","deliveryStatus":[{"status":"DeliveryImpossible"}]}`, []string{"m1:failed"}},
		{"V2 both ids", `{"requestId":"r1","clientCorrelator":"m1","deliveryStatus":[{"status":"DeliveredToNetwork"}]}`, []string{"r1:submitted", "m1:submitted"}},
		{"same ids deduplicated", `{"requestId":"m1","clientCorrelator":"m1","clientCorrelatorId":"m1","status":"DELIVERED"}`, []string{"m1:delivered"}},
		{"OMA nesting", `{"deliveryInfoNotification":{"clientCorrelator":"m1","deliveryInfo":{"address":"233541234567","deliveryStatus":"DeliveredToTerminal"}}}`, []string{"m1:delivered"}},
		{"case insensitive", `{"RequestId":"r1","ClientCorrelator":"m1","DeliveryStatus":[{"Status":"DeliveredToTerminal"}]}`, []string{"r1:delivered", "m1:delivered"}},
	}
	for _, c := range cases {
		got, err := p.ParseStatus(dlrHTTP(testDLRToken, ""), []byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var have []string
		for _, r := range got {
			have = append(have, r.ProviderMessageID+":"+r.Status)
			if r.Provider != Name || !r.At.Equal(fixed) {
				t.Errorf("%s: receipt %+v (a missing or unreadable completedDate falls back to now)", c.name, r)
			}
		}
		if strings.Join(have, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v want %v", c.name, have, c.want)
		}
	}
}

func TestParseStatusErrors(t *testing.T) {
	p := newTestProvider("")
	for name, body := range map[string]string{
		"empty":                  "",
		"not JSON":               "id=1&status=DELIVERED",
		"empty object":           `{}`,
		"no correlator id":       `{"deliveryStatus":"DELIVERED"}`,
		"no status":              `{"clientCorrelatorId":"m1"}`,
		"blank values":           `{"clientCorrelatorId":" ","deliveryStatus":" "}`,
		"id too long":            `{"clientCorrelatorId":"` + strings.Repeat("x", 200) + `","deliveryStatus":"DELIVERED"}`,
		"data is a string":       `{"data":"hello"}`,
		"scalar":                 `42`,
		"truncated JSON":         `{"clientCorrelatorId":"m1",`,
		"empty array":            `[]`,
		"only unusable in array": `[{"nothing":"useful"}]`,
		"trailing object":        `{"requestId":"r1","status":"DELIVERED"}{}`,
		"trailing garbage":       `{"requestId":"r1","status":"DELIVERED"}oops`,
		"excessive nesting":      `{"a":{"b":{"c":{"d":{"e":{"requestId":"r1","status":"DELIVERED"}}}}}}`,
	} {
		if _, err := p.ParseStatus(dlrHTTP(testDLRToken, ""), []byte(body)); !errors.Is(err, channel.ErrBadPayload) {
			t.Errorf("%s: want ErrBadPayload, got %v", name, err)
		}
	}
}

func TestStatusMapping(t *testing.T) {
	// V2 leaves the vocabulary open; cover v3, SMPP and OMA spellings.
	cases := map[string][2]string{
		"DELIVERED":           {"delivered", ""},
		"ACCEPTD":             {"submitted", ""},
		"ENROUTE":             {"submitted", ""},
		"PENDING":             {"submitted", ""},
		"UNDELIVERED":         {"failed", "UNDELIVERED"},
		"DELETED":             {"failed", "DELETED"},
		"REJECTED":            {"rejected", "REJECTED"},
		"EXPIRED":             {"expired", "EXPIRED"},
		"UNKNOWN":             {"unknown", "UNKNOWN"},
		"Some New One":        {"unknown", "SOME NEW ONE"},
		" en-route ":          {"submitted", ""},
		"DeliveredToTerminal": {"delivered", ""},
		"DeliveredToNetwork":  {"submitted", ""},
		"MessageWaiting":      {"submitted", ""},
		"DeliveryImpossible":  {"failed", "DELIVERYIMPOSSIBLE"},
	}
	for in, want := range cases {
		st, code := mapStatus(in)
		if st != want[0] || code != want[1] {
			t.Errorf("mapStatus(%q) = %q, %q; want %q, %q", in, st, code, want[0], want[1])
		}
	}
}

func TestParseStatusAuthentication(t *testing.T) {
	body := fixture(t, "mtn_sms_delivery_notification_constructed.json")
	unconfigured := New(Options{USSDTokens: []string{testUSSDToken}})
	rotated := New(Options{DLRTokens: []string{"old-dlr-token-0123456789abcdef", testDLRToken}})
	cases := []struct {
		name string
		p    *Provider
		r    *http.Request
		ok   bool
	}{
		{"valid token", newTestProvider(""), dlrHTTP(testDLRToken, ""), true},
		{"missing token", newTestProvider(""), dlrHTTP("", ""), false},
		{"wrong token", newTestProvider(""), dlrHTTP(testDLRToken+"x", ""), false},
		{"USSD token does not open delivery notifications", newTestProvider(""), dlrHTTP(testUSSDToken, ""), false},
		{"no delivery token configured", unconfigured, dlrHTTP(testUSSDToken, ""), false},
		{"no delivery token configured, empty", unconfigured, dlrHTTP("", ""), false},
		{"old token during rotation", rotated, dlrHTTP("old-dlr-token-0123456789abcdef", ""), true},
		{"new token during rotation", rotated, dlrHTTP(testDLRToken, ""), true},
		{"token in the body is not enough", newTestProvider(""), httptest.NewRequest(http.MethodPost, DLRCallbackPath, strings.NewReader(`{"token":"`+testDLRToken+`"}`)), false},
	}
	for _, c := range cases {
		_, err := c.p.ParseStatus(c.r, body)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", c.name, err)
		}
	}
	// A failed authentication must not apply anything or leak the cause.
	if _, err := newTestProvider("").ParseStatus(dlrHTTP("", ""), []byte("garbage")); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("unauthenticated garbage: %v", err)
	}
}

func TestParseStatusClipsErrorText(t *testing.T) {
	long := strings.Repeat("é", 100) // two bytes each: the cut must not split a character
	body := `{"clientCorrelatorId":"m1","deliveryStatus":"UNDELIVERED","error":"` + long + `"}`
	got, err := newTestProvider("").ParseStatus(dlrHTTP(testDLRToken, ""), []byte(body))
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if len(got[0].ErrorCode) != 64 || !strings.HasPrefix(long, got[0].ErrorCode) {
		t.Fatalf("error code %q (%d bytes)", got[0].ErrorCode, len(got[0].ErrorCode))
	}
	// A delivered message carries no error code even if MTN sends error text.
	got, err = newTestProvider("").ParseStatus(dlrHTTP(testDLRToken, ""), []byte(`{"clientCorrelatorId":"m1","deliveryStatus":"DELIVERED","error":"none"}`))
	if err != nil || got[0].ErrorCode != "" {
		t.Fatalf("delivered with error text: %+v %v", got, err)
	}
}
