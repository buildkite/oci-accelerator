package peering

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"
)

// NewClient returns a bounded, HTTP/1.1-only client dedicated to peer traffic.
func NewClient(tlsConfig *tls.Config) *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      false,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		TLSNextProto:           make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: rejectRedirect}
}

// Exchange sends one full-list peer exchange request.
func Exchange(ctx context.Context, client *http.Client, destination string, message Message) (Message, error) {
	if client == nil {
		return Message{}, errExchange
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return Message{}, errExchange
	}
	body, err := encodeMessage(message)
	if err != nil {
		return Message{}, errExchange
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+destination+"/v1/peers/exchange", bytes.NewReader(body))
	if err != nil {
		return Message{}, errExchange
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	redirectSafe := *client
	redirectSafe.CheckRedirect = rejectRedirect
	response, err := redirectSafe.Do(request)
	if err != nil {
		return Message{}, errExchange
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || !jsonContentType(response.Header.Get("Content-Type")) {
		return Message{}, errExchange
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxMessageBytes+1))
	if err != nil || len(encoded) > maxMessageBytes {
		return Message{}, errExchange
	}
	result, err := decodeMessage(encoded)
	if err != nil {
		return Message{}, errExchange
	}
	return result, nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
