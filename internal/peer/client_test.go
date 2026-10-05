package peer

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExchangeTwoNodes(t *testing.T) {
	var received Message
	server := httptest.NewTLSServer(NewHandler(func(message Message) (Message, error) {
		received = message
		return Message{Address: "NODE-B.TEST:9443", Peers: []string{"node-c.test:443"}}, nil
	}))
	defer server.Close()

	client := server.Client()
	result, err := Exchange(context.Background(), client, strings.TrimPrefix(server.URL, "https://"), Message{
		Address: "NODE-A.TEST.:8443",
		Peers:   []string{"NODE-D.TEST:443"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Address != "node-a.test:8443" || received.Peers[0] != "node-d.test:443" {
		t.Fatalf("server received %#v", received)
	}
	if result.Address != "node-b.test:9443" || result.Peers[0] != "node-c.test:443" {
		t.Fatalf("client received %#v", result)
	}
}

func TestExchangeRejectsBadResponses(t *testing.T) {
	tests := map[string]http.Handler{
		"status":       http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "secret input", http.StatusBadGateway) }),
		"content type": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }),
		"trailing JSON": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"address":"peer.test:443","peers":[]} {}`)
		}),
		"oversized": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, strings.Repeat(" ", maxMessageBytes+1))
		}),
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			_, err := Exchange(context.Background(), server.Client(), strings.TrimPrefix(server.URL, "https://"), Message{Address: "self.test:443"})
			if err == nil || strings.Contains(err.Error(), "secret input") {
				t.Fatalf("error = %v", err)
			}
		})
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
	_, err := Exchange(context.Background(), client, strings.TrimPrefix(redirect.URL, "https://"), Message{Address: "self.test:443"})
	if err == nil || targetCalled {
		t.Fatalf("error = %v, target called = %t", err, targetCalled)
	}
}

func TestExchangeCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Exchange(ctx, server.Client(), strings.TrimPrefix(server.URL, "https://"), Message{Address: "self.test:443"})
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
