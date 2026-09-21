package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
func TestInterceptForwardsRequestAndRelaysSensitiveResponse(t *testing.T) {
	const query = "signature=do-not-log-this&opaque=%zz;keep"
	var requests atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v2/team/image/manifests/latest?"+query {
			t.Errorf("request = %s %q", r.Method, r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer private-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.oci.image.manifest.v1+json" {
			t.Errorf("Accept = %q", got)
		}
		for _, h := range []string{"Connection", "X-Hop", "Proxy-Authorization", "Proxy-Authenticate", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			if got := r.Header.Get(h); got != "" {
				t.Errorf("%s reached origin: %q", h, got)
			}
		}
		w.Header().Add("WWW-Authenticate", `Bearer realm="https://auth.invalid/token?secret=response-token"`)
		w.Header().Add("Set-Cookie", "registry_session=response-cookie; Secure")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "exact-origin-bytes")
	}))
	defer origin.Close()

	var logs bytes.Buffer
	p, _, client := interceptFixture(t, origin, &logs, true)
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/v2/team/image/manifests/latest?"+query, nil)
	req.Header.Set("Authorization", "Bearer private-token")
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Set("Connection", "keep-alive, X-Hop")
	req.Header.Set("X-Hop", "remove-me")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	req.Header.Set("Forwarded", "for=attacker")
	req.Header.Set("X-Forwarded-For", "attacker")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "exact-origin-bytes" || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response = %d %q, %v", resp.StatusCode, body, err)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "response-token") {
		t.Fatalf("challenge = %q", got)
	}
	if got := resp.Header.Get("Set-Cookie"); !strings.Contains(got, "response-cookie") {
		t.Fatalf("cookie = %q", got)
	}
	if requests.Load() != 1 {
		t.Fatalf("origin requests = %d", requests.Load())
	}
	client.Transport.(*http.Transport).CloseIdleConnections()
	shutdown(t, p)
	for _, secret := range []string{query, "private-token", "proxy-secret", "response-token", "response-cookie"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain secret %q: %s", secret, logs.String())
		}
	}
}

func TestInterceptAuthorityTargetsAndMethods(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-Method", r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	p, proxyAddr, client := interceptFixture(t, origin, nil, true)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req, _ := http.NewRequest(method, origin.URL+"/v2/", nil)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Method") != method {
			t.Fatalf("%s response = %#v, %v", method, resp, err)
		}
		resp.Body.Close()
	}
	baseline := hits.Load()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodTrace} {
		req, _ := http.NewRequest(method, origin.URL+"/v2/", nil)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s response = %#v, %v", method, resp, err)
		}
		resp.Body.Close()
	}
	if hits.Load() != baseline {
		t.Fatalf("denied methods reached origin: %d -> %d", baseline, hits.Load())
	}

	host := strings.TrimPrefix(origin.URL, "https://")
	for _, tc := range []struct {
		target, host string
		want         int
	}{
		{"/v2/", "example.invalid:443", http.StatusMisdirectedRequest},
		{"https://" + host + "/v2/", host, http.StatusBadRequest},
	} {
		resp := rawInterceptRequest(t, proxyAddr.Host, host, tc.host, tc.target, "", client.Transport.(*http.Transport).TLSClientConfig.RootCAs)
		if resp.StatusCode != tc.want {
			t.Errorf("target %q host %q status = %d", tc.target, tc.host, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if hits.Load() != baseline {
		t.Fatalf("invalid authorities reached origin: %d -> %d", baseline, hits.Load())
	}
	shutdown(t, p)
}

func TestInterceptRelaysRedirectWithoutFollowing(t *testing.T) {
	var first, second atomic.Int32
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { second.Add(1) }))
	defer cdn.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first.Add(1)
		w.Header().Set("Location", cdn.URL+"/signed?token=unchanged")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	_, _, client := interceptFixture(t, origin, nil, true)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(origin.URL + "/v2/blob")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != cdn.URL+"/signed?token=unchanged" || first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("redirect = %d %q; calls %d/%d", resp.StatusCode, resp.Header.Get("Location"), first.Load(), second.Load())
	}
}

func TestInterceptPassesRegistryResponseSemanticsAndTagsAlwaysUpstream(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/head":
			w.Header().Set("Content-Length", "12")
		case "/partial":
			w.Header().Set("Content-Range", "bytes 2-4/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "234")
		case "/not-modified":
			w.Header().Set("ETag", `"same"`)
			w.WriteHeader(http.StatusNotModified)
		case "/range-bad":
			w.Header().Set("Content-Range", "bytes */10")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		default:
			_, _ = io.WriteString(w, "tag-version")
		}
	}))
	defer origin.Close()
	_, _, client := interceptFixture(t, origin, nil, true)
	checks := []struct {
		method, path string
		status       int
		body         string
	}{
		{http.MethodHead, "/head", 200, ""}, {http.MethodGet, "/partial", 206, "234"},
		{http.MethodGet, "/not-modified", 304, ""}, {http.MethodGet, "/range-bad", 416, ""},
	}
	for _, tc := range checks {
		req, _ := http.NewRequest(tc.method, origin.URL+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != tc.status || string(body) != tc.body {
			t.Errorf("%s %s = %d %q, %v", tc.method, tc.path, resp.StatusCode, body, readErr)
		}
	}
	before := calls.Load()
	for i := 0; i < 2; i++ {
		resp, err := client.Get(origin.URL + "/v2/repo/manifests/latest")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if calls.Load()-before != 2 {
		t.Fatalf("tag upstream calls = %d", calls.Load()-before)
	}
}

func TestInterceptCancellationCancelsUpstreamAndTruncationErrors(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		cancelled := make(chan struct{})
		entered := make(chan struct{})
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(cancelled)
		}))
		defer origin.Close()
		_, _, client := interceptFixture(t, origin, nil, true)
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/slow", nil)
		result := make(chan error, 1)
		go func() { _, err := client.Do(req); result <- err }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request did not reach upstream")
		}
		cancel()
		if err := <-result; err == nil {
			t.Fatal("cancelled request succeeded")
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("upstream context not cancelled")
		}
	})
	t.Run("truncated response", func(t *testing.T) {
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "20")
			_, _ = io.WriteString(w, "short")
		}))
		defer origin.Close()
		var logs bytes.Buffer
		p, _, client := interceptFixture(t, origin, &logs, true)
		resp, err := client.Get(origin.URL + "/blob")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil || string(body) != "short" {
			t.Fatalf("read = %q, %v", body, err)
		}
		shutdown(t, p)
		if !strings.Contains(logs.String(), "outcome=aborted") || strings.Contains(logs.String(), "outcome=complete") {
			t.Fatalf("truncated response outcome: %s", logs.String())
		}
	})
}

func rawInterceptRequest(t *testing.T, proxyAddr, connectHost, requestHost, target, sni string, roots *x509.CertPool) *http.Response {
	t.Helper()
	conn, _ := connectLifecycle(t, proxyAddr, connectHost, nil)
	if sni == "" {
		sni, _, _ = net.SplitHostPort(connectHost)
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: sni, RootCAs: roots})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tlsConn, "GET "+target+" HTTP/1.1\r\nHost: "+requestHost+"\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
