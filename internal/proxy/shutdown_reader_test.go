package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
	"ampcode.com/buildkite/docker-accelerator/internal/cache"
	"ampcode.com/buildkite/docker-accelerator/internal/config"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

// TestHardTimeoutShutdownReleasesCacheReaderBeforeStoreClose reproduces review
// finding #2: a cache-hit request in flight during a hard-timeout Shutdown must
// have its *cache.Reader closed before the caller closes the store, otherwise
// Store.Close fails with "cache has active readers".
//
// A large blob is served so the warm cache-hit response body cannot be copied
// to a client that stops reading, keeping the reader pinned while Shutdown's
// deadline fires. After Shutdown returns, closing the store must succeed.
func TestHardTimeoutShutdownReleasesCacheReaderBeforeStoreClose(t *testing.T) {
	body := strings.Repeat("cache-hit-payload", 1<<16) // ~1 MiB, exceeds socket buffers
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer origin.Close()

	signer, err := ca.Open(filepath.Join(t.TempDir(), "ca"), 16)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	// store is closed explicitly at the end; no t.Cleanup close so we control
	// the ordering relative to Shutdown.

	u, _ := url.Parse(origin.URL)
	registry, _ := hostport.Parse(u.Host, "443")
	cfg := config.Config{
		Registries:            []hostport.HostPort{registry},
		MaxConnections:        8,
		DialTimeout:           time.Second,
		TLSHandshakeTimeout:   time.Second,
		ResponseHeaderTimeout: time.Second,
		IdleTimeout:           2 * time.Second,
	}
	p := New(cfg, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	l := listen(t)
	go func() { _ = p.Serve(l) }()
	addr := l.Addr().String()

	originRoots := x509.NewCertPool()
	originRoots.AddCert(origin.Certificate())
	p.transport.TLSClientConfig.RootCAs = originRoots

	clientRoots := x509.NewCertPool()
	pemBytes, err := os.ReadFile(signer.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !clientRoots.AppendCertsFromPEM(pemBytes) {
		t.Fatal("append proxy CA")
	}

	// Warm the cache with a normal request that reads the whole body.
	warm := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
			TLSClientConfig:   &tls.Config{RootCAs: clientRoots},
			DisableKeepAlives: true,
		},
		Timeout: 5 * time.Second,
	}
	resp, gotBody := getBlob(t, warm, origin.URL+blobURL(body), "Bearer warm")
	if resp.StatusCode != http.StatusOK || gotBody != body {
		t.Fatalf("warm request failed: status %d, bodyMatch %v", resp.StatusCode, gotBody == body)
	}
	warm.CloseIdleConnections()
	if st := store.Stats(); st.Entries != 1 {
		t.Fatalf("cache not warmed: %+v", st)
	}

	// Open a raw CONNECT+TLS request for the same blob (a cache hit) and read
	// only the response headers, then stop. The large body cannot drain, so the
	// proxy's copy blocks with the cache reader pinned.
	conn, _ := connectLifecycle(t, addr, u.Host, nil)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: registry.Host(), RootCAs: clientRoots})
	// Register cleanup up front so a mid-test t.Fatalf cannot leak the stalled
	// client socket into sibling tests.
	t.Cleanup(func() { _ = tlsConn.Close() })
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tlsConn, "GET "+blobURL(body)+" HTTP/1.1\r\nHost: "+u.Host+"\r\nConnection: close\r\n\r\n")
	stalled, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("reading stalled response headers: %v", err)
	}
	if stalled.StatusCode != http.StatusOK {
		t.Fatalf("stalled cache hit status = %d", stalled.StatusCode)
	}
	// Do not read the body. Wait until a reader is pinned in the store.
	deadline := time.Now().Add(3 * time.Second)
	for store.Stats().Readers == 0 {
		if time.Now().After(deadline) {
			t.Fatal("cache reader never became active for the stalled cache hit")
		}
		time.Sleep(time.Millisecond)
	}

	// Hard-timeout Shutdown: the deadline fires while the reader is pinned. The
	// fix requires Shutdown to wait for the interception handler to unwind (and
	// thus close its cache reader) before returning, so that a store close
	// immediately afterward - exactly as the CLI does via defer - never observes
	// an active reader. Shutdown may report DeadlineExceeded (forced closure) or,
	// if the stalled copy happens to drain first, nil; either way the store must
	// be closeable, which is the property under test.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = p.Shutdown(ctx)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v, want nil or deadline exceeded", err)
	}

	// Close the store the instant Shutdown returns, with no intervening work that
	// would let a still-running handler catch up. Without the drain in Shutdown
	// this observes the pinned reader and fails.
	if r := store.Stats().Readers; r != 0 {
		t.Fatalf("cache readers still active after Shutdown: %d", r)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close after hard-timeout Shutdown: %v", err)
	}
	// Close the proxy listener so its Serve goroutine exits promptly; the stalled
	// client is torn down by the registered cleanup above.
	_ = l.Close()
}
