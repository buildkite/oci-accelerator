package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInterceptTrustFailures(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "must-not-arrive")
	}))
	defer origin.Close()

	t.Run("client does not trust proxy CA", func(t *testing.T) {
		_, _, client := interceptFixture(t, origin, nil, false)
		_, err := client.Get(origin.URL + "/v2/")
		if err == nil {
			t.Fatal("request unexpectedly succeeded")
		}
		if hits.Load() != 0 {
			t.Fatalf("origin hits = %d", hits.Load())
		}
	})
	t.Run("origin certificate is untrusted", func(t *testing.T) {
		_, _, client := interceptFixtureWithOriginTrust(t, origin, true, nil)
		resp, err := client.Get(origin.URL + "/v2/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || bytes.Contains(body, []byte("must-not-arrive")) {
			t.Fatalf("response = %d %q", resp.StatusCode, body)
		}
		if hits.Load() != 0 {
			t.Fatalf("origin hits = %d", hits.Load())
		}
	})
	t.Run("origin certificate name mismatch", func(t *testing.T) {
		roots := x509.NewCertPool()
		roots.AddCert(origin.Certificate())
		p, _, client := interceptFixtureWithOriginTrust(t, origin, true, roots)
		p.transport.TLSClientConfig.ServerName = "wrong.invalid"
		resp, err := client.Get(origin.URL + "/v2/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || bytes.Contains(body, []byte("must-not-arrive")) {
			t.Fatalf("response = %d %q", resp.StatusCode, body)
		}
	})
}

func TestInterceptGeneralOPTIONSIsValidated(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer origin.Close()
	p, proxyAddr, client := interceptFixture(t, origin, nil, true)
	host := strings.TrimPrefix(origin.URL, "https://")
	conn, _ := connectLifecycle(t, proxyAddr.Host, host, nil)
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "127.0.0.1", RootCAs: client.Transport.(*http.Transport).TLSClientConfig.RootCAs})
	defer tlsConn.Close()
	if _, err := io.WriteString(tlsConn, "OPTIONS * HTTP/1.1\r\nHost: other.invalid\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodOptions})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusBadRequest || !resp.Close || string(body) != "invalid request target\n" {
		t.Fatalf("OPTIONS status=%d close=%v body=%q err=%v", resp.StatusCode, resp.Close, body, err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("connection after OPTIONS: %v, want EOF", err)
	}
	awaitNoActiveRequests(t, p)
	if hits.Load() != 0 || p.metrics.snapshot().statuses["4xx"] != 1 {
		t.Fatal("OPTIONS bypassed validation or request instrumentation")
	}
}

func TestInterceptSNIMismatchRejectedBeforeOrigin(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer origin.Close()
	_, proxyAddr, client := interceptFixture(t, origin, nil, true)
	host := strings.TrimPrefix(origin.URL, "https://")
	conn, reader := connectLifecycle(t, proxyAddr.Host, host, nil)
	_ = reader
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "wrong.invalid", RootCAs: client.Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err := tlsConn.Handshake(); err == nil {
		t.Fatal("SNI mismatch handshake succeeded")
	}
	if hits.Load() != 0 {
		t.Fatalf("origin hits = %d", hits.Load())
	}
}

func TestInterceptShutdownClosesTLSKeepAliveWithoutContextDeadline(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer origin.Close()
	p, _, client := interceptFixture(t, origin, nil, true)
	client.Transport.(*http.Transport).DisableKeepAlives = false
	resp, err := client.Get(origin.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown blocked on intercepted keepalive")
	}
}

func TestInterceptRegistersBeforeHandshakeAndCleansUpOnShutdown(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer origin.Close()
	p, addr, _ := interceptFixture(t, origin, nil, true)
	p.cfg.TLSHandshakeTimeout = time.Minute
	conn, _ := connectLifecycle(t, addr.Host, strings.TrimPrefix(origin.URL, "https://"), nil)

	// CONNECT is acknowledged, but the client has not sent a TLS ClientHello.
	// The inner server must already be visible to shutdown at this boundary.
	p.mu.Lock()
	inners := len(p.inners)
	p.mu.Unlock()
	if inners != 1 {
		t.Fatalf("registered inner servers = %d, want 1 before handshake", inners)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Shutdown = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown waited for the TLS handshake timeout")
	}
	assertLifecycleClosed(t, conn, "pending TLS client")
	p.mu.Lock()
	inners, conns := len(p.inners), len(p.conns)
	p.mu.Unlock()
	if inners != 0 || conns != 0 || hits.Load() != 0 {
		t.Fatalf("shutdown cleanup: inner servers=%d connections=%d origin requests=%d", inners, conns, hits.Load())
	}
}
