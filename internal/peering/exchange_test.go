package peering

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHandlerValidatesAndCanonicalizes(t *testing.T) {
	var called atomic.Int32
	handler := NewHandler(func(message Message) (Message, error) {
		called.Add(1)
		want := Message{Address: "node-a.test:443", NodeID: testIDA, Peers: map[string]string{"[2001:db8::1]:8443": testIDC}}
		if !reflect.DeepEqual(message, want) {
			t.Errorf("callback message = %#v, want %#v", message, want)
		}
		return Message{Address: "NODE-B.TEST.:443", NodeID: testIDB, Peers: map[string]string{"NODE-C.TEST:443": testIDC, "NODE-A.TEST:443": testIDA}}, nil
	})

	recorder := serveExchange(handler, `{"address":"NODE-A.TEST.:443","nodeId":"`+testIDA+`","peers":{"[2001:0db8::1]:8443":"`+testIDC+`"}}`)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d, %q", recorder.Code, recorder.Body.String())
	}
	want := `{"address":"node-b.test:443","nodeId":"` + testIDB + `","peers":{"node-a.test:443":"` + testIDA + `","node-c.test:443":"` + testIDC + `"}}`
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if called.Load() != 1 {
		t.Fatalf("callback calls = %d", called.Load())
	}
}

func TestHandlerEncodesEmptyPeersAsObject(t *testing.T) {
	handler := NewHandler(func(Message) (Message, error) {
		return Message{Address: "node-b.test:443", NodeID: testIDB}, nil
	})
	recorder := serveExchange(handler, `{"address":"node-a.test:443","nodeId":"`+testIDA+`","peers":{}}`)
	if want := `{"address":"node-b.test:443","nodeId":"` + testIDB + `","peers":{}}`; recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("response = %d, %q, want %q", recorder.Code, recorder.Body.String(), want)
	}
}

func TestHandlerRejectsMalformedAndOversizedRequestsBeforeCallback(t *testing.T) {
	var called atomic.Int32
	handler := NewHandler(func(message Message) (Message, error) {
		called.Add(1)
		return message, nil
	})
	tooMany := make([]string, MaxPeers+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf(`"p%04d.test:1":"%s"`, i, testIDB)
	}
	id := `"nodeId":"` + testIDA + `"`
	tests := map[string]string{
		"unknown field":          `{"address":"peer.test:443",` + id + `,"peers":{},"extra":true}`,
		"case-variant field":     `{"address":"peer.test:443",` + id + `,"peers":{},"Address":"other.test:443"}`,
		"trailing JSON":          `{"address":"peer.test:443",` + id + `,"peers":{}} {}`,
		"duplicate field":        `{"address":"peer.test:443",` + id + `,"peers":{},"address":"other.test:443"}`,
		"duplicate peer key":     `{"address":"peer.test:443",` + id + `,"peers":{"b.test:1":"` + testIDB + `","b.test:1":"` + testIDC + `"}}`,
		"repeated peer key":      `{"address":"peer.test:443",` + id + `,"peers":{"b.test:1":"` + testIDB + `","b.test:1":"` + testIDB + `"}}`,
		"conflicting alias":      `{"address":"peer.test:443",` + id + `,"peers":{"b.test:1":"` + testIDB + `","B.TEST.:01":"` + testIDC + `"}}`,
		"too many unique peers":  `{"address":"peer.test:443",` + id + `,"peers":{` + strings.Join(tooMany, ",") + `}}`,
		"legacy peer list":       `{"address":"peer.test:443","peers":["b.test:1"]}`,
		"legacy list with ID":    `{"address":"peer.test:443",` + id + `,"peers":["b.test:1"]}`,
		"missing node ID":        `{"address":"peer.test:443","peers":{}}`,
		"invalid node ID":        `{"address":"peer.test:443","nodeId":"short","peers":{}}`,
		"invalid peer node ID":   `{"address":"peer.test:443",` + id + `,"peers":{"b.test:1":"nodeB+ID/456"}}`,
		"numeric node ID":        `{"address":"peer.test:443","nodeId":123456789012,"peers":{}}`,
		"null peers":             `{"address":"peer.test:443",` + id + `,"peers":null}`,
		"missing peers":          `{"address":"peer.test:443",` + id + `}`,
		"nested peer record":     `{"address":"peer.test:443",` + id + `,"peers":{"b.test:1":{"id":"` + testIDB + `"}}}`,
		"invalid sender address": `{"address":"peer.test",` + id + `,"peers":{}}`,
		"oversized":              strings.Repeat(" ", maxMessageBytes+1),
	}
	for name, body := range tests {
		if recorder := serveExchange(handler, body); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", name, recorder.Code)
		}
	}
	if called.Load() != 0 {
		t.Fatalf("callback calls = %d", called.Load())
	}
}

func TestHandlerAcceptsContradictoryClaimAboutSender(t *testing.T) {
	var received Message
	handler := NewHandler(func(message Message) (Message, error) {
		received = message
		return Message{Address: "node-b.test:443", NodeID: testIDB}, nil
	})
	body := `{"address":"node-a.test:443","nodeId":"` + testIDA + `","peers":{"NODE-A.TEST:443":"` + testIDC + `","node-b.test:443":"` + testIDC + `"}}`
	if recorder := serveExchange(handler, body); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if received.NodeID != testIDA || received.Peers["node-a.test:443"] != testIDC || received.Peers["node-b.test:443"] != testIDC {
		t.Fatalf("callback message = %#v", received)
	}
}

func TestHandlerRejectsInvalidResponses(t *testing.T) {
	large := strings.Repeat("a", maxMessageBytes)
	tests := map[string]Message{
		"oversized":         {Address: large + ".test:443", NodeID: testIDB},
		"invalid node ID":   {Address: "node-b.test:443", NodeID: "short"},
		"conflicting alias": {Address: "node-b.test:443", NodeID: testIDB, Peers: map[string]string{"c.test:1": testIDC, "C.TEST:1": testIDA}},
	}
	for name, response := range tests {
		handler := NewHandler(func(Message) (Message, error) { return response, nil })
		recorder := serveExchange(handler, `{"address":"peer.test:443","nodeId":"`+testIDA+`","peers":{}}`)
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d", name, recorder.Code)
		}
	}
}

func serveExchange(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
