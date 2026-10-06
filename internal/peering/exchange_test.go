package peering

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHandlerValidatesAndCanonicalizes(t *testing.T) {
	var called atomic.Int32
	handler := NewHandler(func(message Message) (Message, error) {
		called.Add(1)
		if message.Address != "node-a.test:443" || message.Peers[0] != "[2001:db8::1]:8443" {
			t.Fatalf("callback message = %#v", message)
		}
		return Message{Address: "NODE-B.TEST.:443", Peers: []string{"NODE-A.TEST:443"}}, nil
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", strings.NewReader(`{"address":"NODE-A.TEST.:443","peers":["[2001:0db8::1]:8443"]}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d, %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != `{"address":"node-b.test:443","peers":["node-a.test:443"]}` {
		t.Fatalf("body = %q", got)
	}
	if called.Load() != 1 {
		t.Fatalf("callback calls = %d", called.Load())
	}
}

func TestHandlerRejectsMalformedAndOversizedRequestsBeforeCallback(t *testing.T) {
	var called atomic.Int32
	handler := NewHandler(func(message Message) (Message, error) {
		called.Add(1)
		return message, nil
	})
	tooMany := strings.Repeat(`"peer.test:1",`, MaxPeers+1)
	tests := []string{
		`{"address":"peer.test:443","peers":[],"extra":true}`,
		`{"address":"peer.test:443","peers":[]} {}`,
		`{"address":"peer.test:443","peers":[` + strings.TrimSuffix(tooMany, ",") + `]}`,
		strings.Repeat(" ", maxMessageBytes+1),
	}
	for _, body := range tests {
		request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status = %d for body length %d", recorder.Code, len(body))
		}
	}
	if called.Load() != 0 {
		t.Fatalf("callback calls = %d", called.Load())
	}
}

func TestHandlerBoundsResponse(t *testing.T) {
	large := strings.Repeat("a", maxMessageBytes)
	handler := NewHandler(func(Message) (Message, error) {
		return Message{Address: fmt.Sprintf("%s.test:443", large)}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", strings.NewReader(`{"address":"peer.test:443","peers":[]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
}
