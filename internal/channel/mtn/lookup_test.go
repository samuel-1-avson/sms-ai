package mtn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"smsaighana/internal/channel"
)

func TestSenderExposesLookupOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := New(Options{LookupByClientRef: enabled})
		if _, ok := any(p).(channel.StatusQuerier); ok {
			t.Fatal("Provider must not itself implement StatusQuerier")
		}
		if _, ok := p.Sender().(channel.StatusQuerier); ok != enabled {
			t.Fatalf("lookup enabled=%v, StatusQuerier=%v", enabled, ok)
		}
		if p.Sender().Name() != Name {
			t.Fatal("wrapped sender lost provider name")
		}
	}
}

func TestLookupRequestAndResponse(t *testing.T) {
	fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/messages/sms/outbound/SMS-AI/"+testRequestID+"/deliveryStatus" ||
			r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("x-api-key") != testClientID ||
			r.Header.Get("transactionId") == "" {
			t.Errorf("lookup request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		_, _ = w.Write(fixture(t, "mtn_sms_delivery_status_response_constructed.json"))
	})
	p := New(Options{BaseURL: fake.URL, ClientID: testClientID, ClientSecret: testSecret, SenderID: "SMS-AI", SMSSendsAPIKey: true})
	receipt, ref, err := p.Lookup(context.Background(), testRequestID)
	if err != nil || ref != testMsgID || receipt.Provider != Name || receipt.ProviderMessageID != testRequestID ||
		receipt.Status != "delivered" || receipt.ErrorCode != "" || receipt.At.IsZero() {
		t.Fatalf("lookup: %+v ref=%q err=%v", receipt, ref, err)
	}
	if _, _, err := New(Options{BaseURL: fake.URL}).Lookup(context.Background(), testRequestID); err == nil || fake.apiCalls.Load() != 1 {
		t.Fatalf("missing sender should fail locally: %v", err)
	}
}

func TestQueryStatusRequiresEchoedClientRef(t *testing.T) {
	for _, providerID := range []string{"", testRequestID} {
		for _, echoed := range []string{testMsgID, "", "another-client-ref"} {
			t.Run(providerID+"/"+echoed, func(t *testing.T) {
				fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
					id := providerID
					if id == "" {
						id = testMsgID
					}
					if r.URL.Path != smsStatusPath("SMS-AI", id) {
						t.Errorf("lookup path: %s", r.URL.Path)
					}
					body := fixtureMap(t, "mtn_sms_delivery_status_response_constructed.json")
					body["data"].(map[string]any)["clientCorrelator"] = echoed
					_ = json.NewEncoder(w).Encode(body)
				})
				p := newTestProvider(fake.URL)
				p.opt.LookupByClientRef = true
				receipt, found, err := p.Sender().(channel.StatusQuerier).QueryStatus(context.Background(), testMsgID, providerID)
				if echoed == testMsgID {
					if err != nil || !found || receipt.ProviderMessageID != testRequestID || receipt.Status != "delivered" {
						t.Fatalf("matching reference: %+v found=%v err=%v", receipt, found, err)
					}
				} else if err == nil || found {
					t.Fatalf("unconfirmed reference must be an error: found=%v err=%v", found, err)
				}
			})
		}
	}
}

func TestQueryStatusFailuresNeverMeanNotFound(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"not found", 404, `{}`},
		{"unauthorized", 401, `{}`},
		{"server error", 503, `{}`},
		{"invalid JSON", 200, `oops`},
		{"no delivery status", 200, `{"data":{"requestId":"r1"}}`},
		{"empty status", 200, `{"data":{"clientCorrelator":"` + testMsgID + `","deliveryStatus":[{}]}}`},
		{"canonical error", 200, `{"statusCode":"4004","data":{"clientCorrelator":"` + testMsgID + `","deliveryStatus":[{"status":"DELIVERED"}]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeMTN(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			p := newTestProvider(fake.URL)
			p.opt.LookupByClientRef = true
			_, found, err := p.Sender().(channel.StatusQuerier).QueryStatus(context.Background(), testMsgID, "")
			if err == nil || found {
				t.Fatalf("unsafe not-found or success: found=%v err=%v", found, err)
			}
		})
	}
	p := New(Options{BaseURL: "http://127.0.0.1:1", SenderID: "SMS-AI", LookupByClientRef: true})
	q := p.Sender().(channel.StatusQuerier)
	for _, ref := range []string{"", testMsgID} {
		if _, found, err := q.QueryStatus(context.Background(), ref, ""); err == nil || found {
			t.Fatalf("missing id or transport failure: found=%v err=%v", found, err)
		}
	}
}

func TestSMSPathsEscapeSegments(t *testing.T) {
	if path := smsStatusPath("SMS AI/#", "request/id?"); !strings.Contains(path, "SMS%20AI%2F%23/request%2Fid%3F/") {
		t.Fatalf("unescaped status path: %s", path)
	}
	if path := smsSubscriptionPath("SMS AI/#"); !strings.Contains(path, "SMS%20AI%2F%23/subscription") {
		t.Fatalf("unescaped subscription path: %s", path)
	}
}
