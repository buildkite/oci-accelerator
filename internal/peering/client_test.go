package peering

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExchangeTwoNodes(t *testing.T) {
	var received Message
	server := httptest.NewTLSServer(NewHandler(nil, func(message Message) (Message, error) {
		received = message
		return Message{Address: "NODE-B.TEST:9443", NodeID: testIDB, Peers: map[string]string{"NODE-C.TEST:443": testIDC, "node-a.test:8443": testIDA}}, nil
	}))
	defer server.Close()

	client := server.Client()
	result, err := Exchange(context.Background(), client, strings.TrimPrefix(server.URL, "https://"), Message{
		Address: "NODE-A.TEST.:8443",
		NodeID:  testIDA,
		Peers:   map[string]string{"NODE-D.TEST:443": "caseSensitiv", "node-d.test.:0443": "caseSensitiv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReceived := Message{Address: "node-a.test:8443", NodeID: testIDA, Peers: map[string]string{"node-d.test:443": "caseSensitiv"}}
	if !reflect.DeepEqual(received, wantReceived) {
		t.Fatalf("server received %#v, want %#v", received, wantReceived)
	}
	wantResult := Message{Address: "node-b.test:9443", NodeID: testIDB, Peers: map[string]string{"node-c.test:443": testIDC, "node-a.test:8443": testIDA}}
	if !reflect.DeepEqual(result, wantResult) {
		t.Fatalf("client received %#v, want %#v", result, wantResult)
	}
}

func TestExchangeRejectsInvalidRequestBeforeSending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	tests := map[string]Message{
		"invalid node ID":   {Address: "self.test:443", NodeID: "short"},
		"conflicting alias": {Address: "self.test:443", NodeID: testIDA, Peers: map[string]string{"b.test:1": testIDB, "B.TEST:1": testIDC}},
	}
	for name, message := range tests {
		if _, err := Exchange(context.Background(), server.Client(), strings.TrimPrefix(server.URL, "https://"), message); err == nil {
			t.Errorf("%s: Exchange succeeded", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("server calls = %d", calls.Load())
	}
}

func TestExchangeRejectsBadResponses(t *testing.T) {
	valid := `{"address":"peer.test:443","nodeId":"` + testIDB + `","peers":{}}`
	jsonResponse := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
	}
	tests := map[string]http.Handler{
		"status": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, valid)
		}),
		"content type":       http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, valid) }),
		"trailing JSON":      jsonResponse(valid + ` {}`),
		"unknown field":      jsonResponse(strings.TrimSuffix(valid, "}") + `,"extra":true}`),
		"duplicate peer key": jsonResponse(`{"address":"peer.test:443","nodeId":"` + testIDB + `","peers":{"c.test:1":"` + testIDC + `","c.test:1":"` + testIDA + `"}}`),
		"conflicting alias":  jsonResponse(`{"address":"peer.test:443","nodeId":"` + testIDB + `","peers":{"c.test:1":"` + testIDC + `","C.TEST:1":"` + testIDA + `"}}`),
		"legacy peer list":   jsonResponse(`{"address":"peer.test:443","peers":[]}`),
		"invalid node ID":    jsonResponse(`{"address":"peer.test:443","nodeId":"short","peers":{}}`),
		"oversized":          jsonResponse(valid + strings.Repeat(" ", maxMessageBytes+1)),
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			_, err := Exchange(context.Background(), server.Client(), strings.TrimPrefix(server.URL, "https://"), Message{Address: "self.test:443", NodeID: testIDA})
			if err == nil || strings.Contains(err.Error(), "peer.test") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExchangeAcceptsValidResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"address":"peer.test:443","nodeId":"`+testIDB+`","peers":{}}`)
	}))
	defer server.Close()
	result, err := Exchange(context.Background(), server.Client(), strings.TrimPrefix(server.URL, "https://"), Message{Address: "self.test:443", NodeID: testIDA})
	if err != nil || result.NodeID != testIDB || result.Peers == nil {
		t.Fatalf("Exchange() = %#v, %v", result, err)
	}
}

func TestExchangeDoesNotFollowRedirects(t *testing.T) {
	var targetCalled bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := redirect.Client()
	client.CheckRedirect = nil
	_, err := Exchange(context.Background(), client, strings.TrimPrefix(redirect.URL, "https://"), Message{Address: "self.test:443", NodeID: testIDA})
	if err == nil || targetCalled {
		t.Fatalf("error = %v, target called = %t", err, targetCalled)
	}
}

func TestExchangeCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Exchange(ctx, server.Client(), strings.TrimPrefix(server.URL, "https://"), Message{Address: "self.test:443", NodeID: testIDA})
	if err == nil {
		t.Fatal("Exchange succeeded with canceled context")
	}
}

func TestNewClientIsDedicatedAndHTTP1(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:1234")
	client := NewClient(&tls.Config{MinVersion: tls.VersionTLS12})
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil {
		t.Fatalf("transport = %#v", client.Transport)
	}
	if client.Timeout != 5*time.Second || client.CheckRedirect == nil {
		t.Fatalf("client timeout = %v, redirect policy present = %t", client.Timeout, client.CheckRedirect != nil)
	}
}
