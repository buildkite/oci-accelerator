package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func start(t *testing.T, cfg config.Config) (*Proxy, string) {
	t.Helper()
	l := listen(t)
	p := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	serveProxy(t, p, l)
	return p, l.Addr().String()
}

// serveProxy owns the serving goroutine and checks both lifecycle results.
func serveProxy(t *testing.T, p *Proxy, l net.Listener) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = p.Serve(l)
		p.mu.Lock()
		closing := p.closing
		p.mu.Unlock()
		if serveErr == nil && !closing {
			serveErr = fmt.Errorf("Serve returned before Shutdown")
		}
		close(done)
	}()
	t.Cleanup(func() {
		shutdown(t, p)
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("Serve: %v", serveErr)
			}
		case <-time.After(time.Second):
			t.Error("Serve did not return after Shutdown")
		}
	})
	return done
}

func shutdown(t *testing.T, p *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

type proxyAddress struct{ Host string }

func interceptFixture(t *testing.T, origin *httptest.Server, logs *bytes.Buffer, trustClient bool) (*Proxy, proxyAddress, *http.Client) {
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	return interceptFixtureWithOriginTrustAndLogs(t, origin, trustClient, roots, logs)
}

func interceptFixtureWithOriginTrust(t *testing.T, origin *httptest.Server, trustClient bool, originRoots *x509.CertPool) (*Proxy, proxyAddress, *http.Client) {
	return interceptFixtureWithOriginTrustAndLogs(t, origin, trustClient, originRoots, nil)
}

func interceptFixtureWithOriginTrustAndLogs(t *testing.T, origin *httptest.Server, trustClient bool, originRoots *x509.CertPool, logs *bytes.Buffer) (*Proxy, proxyAddress, *http.Client) {
	t.Helper()
	signer, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(origin.URL)
	registry, err := hostport.Parse(u.Host, "443")
	if err != nil {
		t.Fatal(err)
	}
	p, addr := startSignedProxy(t, config.Config{Registries: []hostport.HostPort{registry}, MaxConnections: 32, DialTimeout: time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: 300 * time.Millisecond, IdleTimeout: 150 * time.Millisecond}, signer, logs)
	p.transport.TLSClientConfig.RootCAs = originRoots
	clientRoots := x509.NewCertPool()
	if trustClient {
		pemBytes, err := os.ReadFile(signer.CertificatePath())
		if err != nil {
			t.Fatal(err)
		}
		if !clientRoots.AppendCertsFromPEM(pemBytes) {
			t.Fatal("append proxy CA")
		}
	}
	proxyURL, _ := url.Parse("http://" + addr)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientRoots}, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return p, proxyAddress{addr}, &http.Client{Transport: tr, Timeout: 2 * time.Second}
}

func startSignedProxy(t *testing.T, cfg config.Config, signer *ca.Authority, logs *bytes.Buffer) (*Proxy, string) {
	t.Helper()
	l := listen(t)
	var output io.Writer = io.Discard
	if logs != nil {
		output = logs
	}
	p := New(cfg, signer, slog.New(slog.NewTextHandler(output, nil)), nil)
	serveProxy(t, p, l)
	return p, l.Addr().String()
}

func connectLifecycle(t *testing.T, proxyAddr, target string, payload []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn := dialLifecycle(t, proxyAddr)
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := io.WriteString(conn, request+string(payload)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("CONNECT status = %d", response.StatusCode)
	}
	return conn, reader
}

func dialLifecycle(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func assertLifecycleClosed(t *testing.T, conn net.Conn, name string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("%s remained open", name)
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("%s was not closed: %v", name, err)
	}
}

func lifecycleConnCount(p *Proxy) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

const testBlobDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testBlobPath = "/v2/team/image/blobs/sha256:" + strings.Repeat("a", 64)

func cacheFixture(t *testing.T, origin *httptest.Server, limit int64, logs *bytes.Buffer) (*Proxy, *http.Client) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	s, err := cachetest.Open(dir, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	p, _, c := interceptFixture(t, origin, logs, true)
	p.cache = s
	p.cfg.Cache.Dir = dir
	return p, c
}

func cachePolicyFixture() (*http.Request, *http.Response) {
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example/v2/team/image/blobs/"+testBlobDigest, nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		ContentLength: 3,
	}
	resp.Header.Set("Content-Type", "application/octet-stream")
	resp.Header.Set("Content-Encoding", "identity")
	return req, resp
}

func getBlob(t *testing.T, c *http.Client, u, token string) (*http.Response, string) {
	t.Helper()
	r, _ := http.NewRequest("GET", u, nil)
	r.Header.Set("Authorization", token)
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func blobURL(body string) string {
	return fmt.Sprintf("/v2/team/image/blobs/sha256:%x", sha256.Sum256([]byte(body)))
}

func blobHeaders(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
}

func manifestURL(repository, body string) string {
	return fmt.Sprintf("/v2/%s/manifests/sha256:%x", repository, sha256.Sum256([]byte(body)))
}

func manifestHeaders(w http.ResponseWriter, body, mediaType string) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
}

const waitTestTimeout = 3 * time.Second

func waitRequest(t *testing.T, c *http.Client, url, token, accept string, ctx context.Context) (*http.Response, []byte, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	r.Header.Set("Authorization", token)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	resp, err := c.Do(r)
	if err != nil {
		return nil, nil, err
	}
	b, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp, b, readErr
	}
	return resp, b, closeErr
}

func waitForFills(t *testing.T, p *Proxy, want int) {
	t.Helper()
	if waitUntil(waitTestTimeout, func() bool {
		return p.cache.Stats().Fills == want
	}) {
		return
	}
	t.Fatalf("timed out waiting for %d active cache fills (got %d)", want, p.cache.Stats().Fills)
}

func awaitNoActiveRequests(t *testing.T, p *Proxy) {
	t.Helper()
	var active int64
	if waitUntil(3*time.Second, func() bool {
		p.metrics.mu.Lock()
		active = p.metrics.activeRequests
		p.metrics.mu.Unlock()
		return active == 0
	}) {
		return
	}
	t.Fatalf("metrics request completion was not recorded (got %d active requests)", active)
}

func eventuallyLifecycle(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	if waitUntil(timeout, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func safeRelease(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// syncBuffer is a concurrency-safe log sink for asserting on structured logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
