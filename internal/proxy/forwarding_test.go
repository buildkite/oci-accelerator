package proxy

import (
	"bufio"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestInformationalResponseWriteDeadline(t *testing.T) {
	for _, reading := range []bool{false, true} {
		name := "non-reading-client"
		if reading {
			name = "reading-client"
		}
		t.Run(name, func(t *testing.T) {
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", "</layer>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				w.Header().Del("Link")
				_, _ = io.WriteString(w, "origin bytes")
			}))
			defer origin.Close()
			u, err := url.Parse(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			target, err := hostport.Parse(u.Host, "443")
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Registries: []hostport.HostPort{target}, DialTimeout: time.Second,
				TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: 100 * time.Millisecond}
			p := New(cfg, nil, nil, nil)
			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			p.transport.TLSClientConfig.RootCAs = roots
			defer p.transport.CloseIdleConnections()

			// A pipe has no send buffer: forwarding even a small 103 blocks until
			// the client reads, without relying on TCP buffer sizes or flooding.
			serverConn, clientConn := net.Pipe()
			listener := &oneConnListener{conn: serverConn, done: make(chan struct{})}
			handlerDone := make(chan struct{})
			connectionClosed := make(chan struct{})
			handler := p.interceptHandler(target)
			server := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(handlerDone)
					handler.ServeHTTP(w, r)
				}),
				ReadHeaderTimeout: cfg.ResponseHeaderTimeout, IdleTimeout: cfg.IdleTimeout,
				ConnState: func(_ net.Conn, state http.ConnState) {
					if state == http.StateClosed {
						close(connectionClosed)
					}
				},
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = server.Serve(listener)
			}()
			defer func() {
				_ = clientConn.Close()
				_ = server.Close()
				<-serveDone
			}()
			if err := clientConn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(clientConn, "GET /v2/ HTTP/1.1\r\nHost: "+u.Host+"\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if reading {
				reader := bufio.NewReader(clientConn)
				hints, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				_ = hints.Body.Close()
				if hints.StatusCode != http.StatusEarlyHints || hints.Header.Get("Link") != "</layer>; rel=preload" {
					t.Fatalf("informational response = %d, %v", hints.StatusCode, hints.Header)
				}
				response, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || string(body) != "origin bytes" || response.Header.Get("Link") != "" {
					t.Fatalf("final response = %d, %v, %q, %v", response.StatusCode, response.Header, body, err)
				}
			}
			select {
			case <-handlerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not finish within the write-timeout allowance")
			}
			select {
			case <-connectionClosed:
			case <-time.After(time.Second):
				t.Fatal("downstream connection was not released")
			}
		})
	}
}
