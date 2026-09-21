package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestShutdownClosesActiveTunnelAtDeadline(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Minute})
	conn, _ := net.Dial("tcp", addr)
	t.Cleanup(func() { conn.Close() })
	target := origin.Addr().String()
	_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || response.StatusCode != 200 {
		t.Fatal(err)
	}
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client remained open")
	}
	upstream.Close()
}

func TestShutdownForceClosesUpstreamAndPumpsWithinDeadline(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Minute})
	client, _ := connectLifecycle(t, addr, origin.Addr().String(), nil)
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 700*time.Millisecond {
		t.Fatalf("Shutdown took %v", elapsed)
	}
	assertLifecycleClosed(t, client, "client")
	assertLifecycleClosed(t, upstream, "upstream")
	eventuallyLifecycle(t, time.Second, func() bool { return lifecycleConnCount(p) == 0 }, "relay pumps exited")
}

func TestShutdownJoinsHalfClosedTunnel(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Minute})
	client, _ := connectLifecycle(t, addr, origin.Addr().String(), []byte("request"))
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = upstream.SetReadDeadline(time.Now().Add(time.Second))
	if body, err := io.ReadAll(upstream); err != nil || string(body) != "request" {
		t.Fatalf("half-closed request = %q, %v", body, err)
	}
	// The client-to-upstream pump has sent FIN. The other pump is waiting for
	// this silent upstream, so closing only the client cannot release it.
	p.mu.Lock()
	var proxyUpstream net.Conn
	for conn := range p.tunnels {
		proxyUpstream = conn
	}
	p.mu.Unlock()
	if proxyUpstream == nil {
		t.Fatal("upstream tunnel was not tracked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown = %v", err)
		}
	case <-time.After(time.Second):
		upstream.Close()
		<-done
		t.Fatal("Shutdown did not release the half-closed tunnel")
	}
	p.mu.Lock()
	tunnels := len(p.tunnels)
	p.mu.Unlock()
	if tunnels != 0 || lifecycleConnCount(p) != 0 {
		t.Fatal("Shutdown returned before tunnel cleanup")
	}
	_ = proxyUpstream.SetReadDeadline(time.Now())
	if _, err := proxyUpstream.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("proxy upstream still open after Shutdown: %v", err)
	}
}

func TestShutdownWaitsForHandlerAfterForcedClose(t *testing.T) {
	for _, alreadyClosed := range []bool{false, true} {
		name := "active connection"
		if alreadyClosed {
			name = "connection already removed"
		}
		t.Run(name, func(t *testing.T) {
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			defer origin.Close()
			p, _, client := interceptFixture(t, origin, nil, true)
			logs := &shutdownLogWriter{started: make(chan struct{}), release: make(chan struct{})}
			p.logger = slog.New(slog.NewTextHandler(logs, nil))
			// Unblock the handler even if an assertion fails before normal release.
			defer func() {
				select {
				case <-logs.release:
				default:
					close(logs.release)
				}
			}()
			requestDone := make(chan struct{})
			go func() {
				defer close(requestDone)
				resp, err := client.Get(origin.URL + "/v2/")
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
			waitFor(t, logs.started, "handler blocked in logger")
			if alreadyClosed {
				p.closeAll()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan error, 1)
			go func() { done <- p.Shutdown(ctx) }()
			eventuallyLifecycle(t, time.Second, func() bool {
				return lifecycleConnCount(p) == 0
			}, "forced client closure")
			// Exceed the former five-second grace period on the forced-close
			// path; even then Shutdown must wait for actual handler completion.
			wait := 5200 * time.Millisecond
			if alreadyClosed {
				wait = 50 * time.Millisecond
			}
			select {
			case err := <-done:
				t.Fatalf("Shutdown returned while handler was blocked: %v", err)
			case <-time.After(wait):
			}
			close(logs.release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Shutdown = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Shutdown did not finish after handler release")
			}
			waitFor(t, requestDone, "request completion")
		})
	}
}

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
	store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<30)
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
	serveProxy(t, p, l)
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

func TestShutdownCancelsCacheFillLeaderAndWaitingFollower(t *testing.T) {
	const body = "never completed cache body"
	leaderHeaders := make(chan struct{})
	followerClosed := make(chan struct{})
	leaderCanceled := make(chan struct{})
	var calls atomic.Int32
	var followerOnce, leaderOnce sync.Once
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(leaderHeaders)
			<-r.Context().Done()
			leaderOnce.Do(func() { close(leaderCanceled) })
			return
		}
		<-r.Context().Done()
		followerOnce.Do(func() { close(followerClosed) })
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.Cache.FillWaitTimeout = time.Minute

	results := make(chan error, 2)
	request := func(token string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, err := waitRequest(t, client, origin.URL+blobURL(body), token, "", ctx)
		results <- err
	}
	go request("Bearer leader")
	waitFor(t, leaderHeaders, "leader headers")
	waitForFills(t, p, 1)
	go request("Bearer follower")
	waitFor(t, followerClosed, "follower upstream body close")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := p.Shutdown(ctx)
	cancel()
	if err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v, want deadline exceeded", err)
	}
	waitFor(t, leaderCanceled, "leader origin cancellation")
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("client unexpectedly received a complete response")
			}
		case <-time.After(waitTestTimeout):
			t.Fatal("timed out waiting for client shutdown error")
		}
	}
	eventuallyLifecycle(t, time.Second, func() bool {
		st := p.cache.Stats()
		return st.Fills == 0 && st.Reserved == 0 && st.Entries == 0 && lifecycleConnCount(p) == 0
	}, "shutdown cleanup without partial cache entry")
}

type shutdownLogWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *shutdownLogWriter) Write(b []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(b), nil
}
