package peering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAvailabilityHandlerAcceptsBatch(t *testing.T) {
	var received []availabilityMessage
	handler := newAvailabilityHandler(func(message availabilityMessage) error {
		received = append(received, message)
		return nil
	})
	claim := availabilityWireClaim(1)
	body := fmt.Sprintf(`{"sender":"NODE-A.TEST.:8443","claims":[%s]}`, availabilityWireJSON(t, claim))
	response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json; charset=utf-8", body)

	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	want := availabilityMessage{Sender: "node-a.test:8443", Claims: []Claim{claim}}
	if len(received) != 1 || !reflect.DeepEqual(received[0], want) {
		t.Fatalf("received %#v, want %#v", received, want)
	}
}

func TestAvailabilityHandlerAcceptsMaximumBatch(t *testing.T) {
	var count int
	handler := newAvailabilityHandler(func(message availabilityMessage) error {
		count = len(message.Claims)
		return nil
	})
	body := availabilityWireBody(t, "node-a.test:443", maxClaimBatch)
	response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json", body)
	if response.Code != http.StatusNoContent || count != maxClaimBatch {
		t.Fatalf("status = %d, claims = %d", response.Code, count)
	}
}

func TestAvailabilityHandlerRejectsInvalidEnvelope(t *testing.T) {
	valid := fmt.Sprintf(`{"sender":"node-a.test:443","claims":[%s]}`, availabilityWireJSON(t, availabilityWireClaim(1)))
	tests := map[string]struct {
		method, target, contentType, body string
	}{
		"GET method":           {http.MethodGet, availabilityPath, "application/json", valid},
		"PUT method":           {http.MethodPut, availabilityPath, "application/json", valid},
		"query":                {http.MethodPost, availabilityPath + "?x=1", "application/json", valid},
		"trailing slash":       {http.MethodPost, availabilityPath + "/", "application/json", valid},
		"other path":           {http.MethodPost, "/v1/peers/exchange", "application/json", valid},
		"escaped path":         {http.MethodPost, "/v1/cache/%61vailability", "application/json", valid},
		"text content type":    {http.MethodPost, availabilityPath, "text/plain", valid},
		"missing content type": {http.MethodPost, availabilityPath, "", valid},
		"malformed JSON":       {http.MethodPost, availabilityPath, "application/json", `{"sender":`},
		"trailing JSON":        {http.MethodPost, availabilityPath, "application/json", valid + ` {}`},
		"trailing garbage":     {http.MethodPost, availabilityPath, "application/json", valid + `x`},
		"unknown field":        {http.MethodPost, availabilityPath, "application/json", `{"sender":"node-a.test:443","claims":[],"extra":1}`},
		"unknown claim field":  {http.MethodPost, availabilityPath, "application/json", `{"sender":"node-a.test:443","claims":[{"owner":"node-a.test:443","bogus":true}]}`},
		"invalid sender":       {http.MethodPost, availabilityPath, "application/json", `{"sender":"no-port.test","claims":[]}`},
		"missing sender":       {http.MethodPost, availabilityPath, "application/json", `{"claims":[]}`},
		"claims not array":     {http.MethodPost, availabilityPath, "application/json", `{"sender":"node-a.test:443","claims":{}}`},
		"too many claims":      {http.MethodPost, availabilityPath, "application/json", availabilityWireBody(t, "node-a.test:443", maxClaimBatch+1)},
		"oversized body":       {http.MethodPost, availabilityPath, "application/json", valid + strings.Repeat(" ", maxMessageBytes)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var called bool
			handler := newAvailabilityHandler(func(availabilityMessage) error {
				called = true
				return nil
			})
			response := serveAvailability(handler, test.method, test.target, test.contentType, test.body)
			if response.Code != http.StatusBadRequest || called {
				t.Fatalf("status = %d, callback called = %t", response.Code, called)
			}
		})
	}
}

func TestAvailabilityHandlerMapsCallbackErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
	}{
		"invalid claims":  {fmt.Errorf("claim 3: %w", errAvailability), http.StatusBadRequest},
		"internal secret": {errors.New("secret detail"), http.StatusServiceUnavailable},
	}
	body := availabilityWireBody(t, "node-a.test:443", 1)
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			handler := newAvailabilityHandler(func(availabilityMessage) error { return test.err })
			response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json", body)
			if response.Code != test.status || strings.Contains(response.Body.String(), "secret") {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestSendAvailabilityDeliversCanonicalBatch(t *testing.T) {
	var received []availabilityMessage
	var request struct{ method, uri, host, contentType string }
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request.method, request.uri, request.contentType = r.Method, r.RequestURI, r.Header.Get("Content-Type")
		newAvailabilityHandler(func(message availabilityMessage) error {
			received = append(received, message)
			return nil
		}).ServeHTTP(w, r)
	}))
	defer server.Close()
	client, hosts := availabilityRewriteClient(server)

	claim := availabilityWireClaim(7)
	err := sendAvailability(context.Background(), client, "PEER-B.TEST.:9443", availabilityMessage{
		Sender: "NODE-A.TEST.:8443",
		Claims: []Claim{claim},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(); !reflect.DeepEqual(got, []string{"peer-b.test:9443"}) {
		t.Fatalf("dialed hosts = %v", got)
	}
	if request.method != http.MethodPost || request.uri != availabilityPath || request.contentType != "application/json" {
		t.Fatalf("request = %#v", request)
	}
	want := availabilityMessage{Sender: "node-a.test:8443", Claims: []Claim{claim}}
	if len(received) != 1 || !reflect.DeepEqual(received[0], want) {
		t.Fatalf("received %#v, want %#v", received, want)
	}
}

func TestSendAvailabilityRejectsInvalidInputWithoutRequest(t *testing.T) {
	oversized := make([]Claim, maxClaimBatch)
	for i := range oversized {
		oversized[i] = availabilityWireClaim(uint64(i))
		oversized[i].Key = strings.Repeat("a", maxMessageBytes/maxClaimBatch)
	}
	tests := map[string]struct {
		nilClient   bool
		destination string
		message     availabilityMessage
	}{
		"nil client":          {nilClient: true, destination: "peer.test:443", message: availabilityMessage{Sender: "self.test:443"}},
		"invalid destination": {destination: "peer.test", message: availabilityMessage{Sender: "self.test:443"}},
		"path destination":    {destination: "peer.test:443/evil", message: availabilityMessage{Sender: "self.test:443"}},
		"invalid sender":      {destination: "peer.test:443", message: availabilityMessage{Sender: "self.test"}},
		"too many claims":     {destination: "peer.test:443", message: availabilityMessage{Sender: "self.test:443", Claims: make([]Claim, maxClaimBatch+1)}},
		"oversized body":      {destination: "peer.test:443", message: availabilityMessage{Sender: "self.test:443", Claims: oversized}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client, _ := availabilityRewriteClient(server)
			if test.nilClient {
				client = nil
			}
			err := sendAvailability(context.Background(), client, test.destination, test.message)
			if err == nil || requests.Load() != 0 {
				t.Fatalf("error = %v, requests = %d", err, requests.Load())
			}
		})
	}
}

func TestSendAvailabilityRequiresNoContentAck(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"OK with body": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secret ok") },
		"bad request":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "secret input", http.StatusBadRequest) },
		"unavailable": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret detail", http.StatusServiceUnavailable)
		},
		"accepted": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) },
		"no response": func(_ http.ResponseWriter, _ *http.Request) {
			panic(http.ErrAbortHandler)
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(handler)
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			defer server.Close()
			client, _ := availabilityRewriteClient(server)
			err := sendAvailability(context.Background(), client, "peer.test:443", availabilityMessage{Sender: "self.test:443"})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSendAvailabilityDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+availabilityPath, status)
			}))
			defer redirect.Close()
			client := redirect.Client()
			client.CheckRedirect = nil
			err := sendAvailability(context.Background(), client, strings.TrimPrefix(redirect.URL, "https://"), availabilityMessage{Sender: "self.test:443"})
			if err == nil || targetCalls.Load() != 0 {
				t.Fatalf("error = %v, target calls = %d", err, targetCalls.Load())
			}
			if client.CheckRedirect != nil {
				t.Fatal("sendAvailability mutated the caller's client")
			}
		})
	}
}

func TestSendAvailabilityCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consuming the body lets the server notice the client closing the connection.
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := availabilityRewriteClient(server)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- sendAvailability(ctx, client, "peer.test:443", availabilityMessage{Sender: "self.test:443"})
	}()
	<-entered
	cancel()
	if err := <-result; err == nil {
		t.Fatal("sendAvailability succeeded after cancellation")
	}
}

func TestSendAvailabilityCanceledBeforeSend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := availabilityRewriteClient(server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sendAvailability(ctx, client, "peer.test:443", availabilityMessage{Sender: "self.test:443"})
	if err == nil || requests.Load() != 0 {
		t.Fatalf("error = %v, requests = %d", err, requests.Load())
	}
}

// availabilityRewriteTransport records the requested authority, then sends the
// request to the local test server so canonical destinations stay hermetic.
type availabilityRewriteTransport struct {
	base   http.RoundTripper
	target string
	hosts  chan string
}

func (t *availabilityRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	select {
	case t.hosts <- request.URL.Host:
	default:
	}
	rewritten := request.Clone(request.Context())
	rewritten.URL.Host = t.target
	rewritten.Host = t.target
	return t.base.RoundTrip(rewritten)
}

func availabilityRewriteClient(server *httptest.Server) (*http.Client, func() []string) {
	transport := &availabilityRewriteTransport{
		base:   server.Client().Transport,
		target: strings.TrimPrefix(server.URL, "https://"),
		hosts:  make(chan string, 16),
	}
	hosts := func() []string {
		var result []string
		for {
			select {
			case host := <-transport.hosts:
				result = append(result, host)
			default:
				return result
			}
		}
	}
	return &http.Client{Transport: transport, CheckRedirect: rejectRedirect}, hosts
}

func serveAvailability(handler http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func availabilityWireBody(t *testing.T, sender string, claims int) string {
	t.Helper()
	message := availabilityMessage{Sender: sender, Claims: make([]Claim, claims)}
	for i := range message.Claims {
		message.Claims[i] = availabilityWireClaim(uint64(i))
	}
	return availabilityWireJSON(t, message)
}

// availabilityWireClaim is opaque to the transport, which never validates claim
// semantics, so its field values only need to round-trip.
func availabilityWireClaim(revision uint64) Claim {
	return Claim{
		Owner:    "node-a.test:443",
		Session:  strings.Repeat("0", 32),
		Key:      strings.Repeat("a", 64),
		Revision: revision,
		Present:  true,
		Expires:  1_700_000_000_000,
	}
}

func availabilityWireJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
