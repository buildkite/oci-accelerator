package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/ca"
	"ampcode.com/buildkite/docker-accelerator/internal/config"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

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

func TestInterceptUpstreamKeepAliveReadDeadlineRefreshesForNextRequest(t *testing.T) {
	const (
		firstBody  = "first"
		secondBody = "second"
	)
	var (
		requests atomic.Int32
		mu       sync.Mutex
		remotes  []string
	)
	releaseSecond := make(chan struct{})
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		mu.Lock()
		remotes = append(remotes, r.RemoteAddr)
		mu.Unlock()
		switch n {
		case 1:
			_, _ = io.WriteString(w, firstBody)
		default:
			<-releaseSecond
			_, _ = io.WriteString(w, secondBody)
		}
	}))
	defer origin.Close()
	release := safeRelease(releaseSecond)
	defer release()

	p, _, client := interceptFixture(t, origin, nil, true)
	const idle = 800 * time.Millisecond
	p.cfg.IdleTimeout = idle
	p.transport.IdleConnTimeout = idle
	p.transport.ResponseHeaderTimeout = idle

	readDeadlines := make(chan time.Time, 32)
	dialIP := p.dialIP
	p.dialIP = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialIP(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &observedReadDeadlineConn{Conn: conn, deadlines: readDeadlines}, nil
	}

	get := func(path string) (string, error) {
		resp, err := client.Get(origin.URL + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}
	if body, err := get("/v2/first"); err != nil || body != firstBody {
		t.Fatalf("first GET: body=%q err=%v", body, err)
	}

	// The transport's idle pool reader has already installed this absolute
	// deadline. Start the next request comfortably before it, but hold its
	// response until after it. A reused connection must refresh the read
	// deadline for the active request rather than replaying the GET.
	var pendingDeadline time.Time
	deadlineWait := time.NewTimer(time.Second)
	defer deadlineWait.Stop()
	for pendingDeadline.IsZero() {
		select {
		case d := <-readDeadlines:
			if time.Until(d) > idle/2 {
				pendingDeadline = d
			}
		case <-deadlineWait.C:
			t.Fatal("upstream transport did not enter its pending keep-alive read")
		}
	}
	if wait := time.Until(pendingDeadline.Add(-idle / 3)); wait > 0 {
		time.Sleep(wait)
	}

	type result struct {
		body string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		body, err := get("/v2/second")
		resultCh <- result{body: body, err: err}
	}()
	for requests.Load() < 2 {
		if time.Now().After(pendingDeadline) {
			t.Fatal("second GET did not reach upstream before stale read deadline")
		}
		time.Sleep(time.Millisecond)
	}
	if wait := time.Until(pendingDeadline.Add(150 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	release()

	res := <-resultCh
	if res.err != nil || res.body != secondBody {
		t.Errorf("second GET: body=%q err=%v", res.body, res.err)
	}
	mu.Lock()
	gotRemotes := append([]string(nil), remotes...)
	mu.Unlock()
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream GET count = %d, want 2 (requests from %v)", got, gotRemotes)
	}
	if len(gotRemotes) >= 2 && gotRemotes[0] != gotRemotes[1] {
		t.Errorf("second GET did not reuse upstream connection: remotes = %v", gotRemotes)
	}
}

type observedReadDeadlineConn struct {
	net.Conn
	deadlines chan<- time.Time
}

func (c *observedReadDeadlineConn) SetReadDeadline(deadline time.Time) error {
	select {
	case c.deadlines <- deadline:
	default:
	}
	return c.Conn.SetReadDeadline(deadline)
}

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

func TestInterceptUpstreamIdleBodyStallErrors(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer origin.Close()
	_, _, client := interceptFixture(t, origin, nil, true)
	start := time.Now()
	resp, err := client.Get(origin.URL + "/stalled")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("stalled read = %v after %v", err, time.Since(start))
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

func TestUnconfiguredTunnelPresentsOriginCertificate(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	signer, err := ca.Open(t.TempDir()+"/ca", 8)
	if err != nil {
		t.Fatal(err)
	}
	p, addr := startSignedProxy(t, config.Config{MaxConnections: 8, DialTimeout: time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second}, signer, nil)
	host := strings.TrimPrefix(origin.URL, "https://")
	conn, _ := connectLifecycle(t, addr, host, nil)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tlsConn.ConnectionState().PeerCertificates[0].Raw, origin.Certificate().Raw) {
		t.Fatal("tunnel presented proxy-signed certificate instead of origin certificate")
	}
	tlsConn.Close()
	shutdown(t, p)
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
	go func() { _ = p.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p, l.Addr().String()
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
