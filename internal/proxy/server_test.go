package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestRefusals(t *testing.T) {
	configured, _ := hostport.Parse("127.0.0.1:9", "443")
	p, addr := start(t, config.Config{Registries: []hostport.HostPort{configured}, MaxConnections: 4, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second})
	for _, tc := range []struct {
		request string
		status  int
	}{
		{"GET / HTTP/1.1\r\nHost: example.com\r\n\r\n", 405},
		{"CONNECT 127.0.0.1:9 HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n", 503},
		{"CONNECT example.com:443 HTTP/1.1\r\nHost: other.example:443\r\n\r\n", 400},
		{"CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n", 400},
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		_, _ = io.WriteString(conn, tc.request)
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || response.StatusCode != tc.status {
			t.Errorf("status for %q = %#v, %v", tc.request, response, err)
		}
		if response != nil {
			response.Body.Close()
		}
		conn.Close()
	}
	shutdown(t, p)
}

func FuzzValidateCONNECT(f *testing.F) {
	for _, seed := range []struct {
		target, host    string
		contentLength   int64
		transferEncoded bool
	}{
		{"example.com:443", "example.com:443", 0, false},
		{"EXAMPLE.COM.:0443", "example.com:443", 0, false},
		{"[::1]:5000", "[::1]:5000", 0, false},
		{"example.com:443", "other.example:443", 0, false},
		{"example.com:443", "example.com:8443", 0, false},
		{"example.com", "example.com", 0, false},
		{"user@host:1", "user@host:1", 0, false},
		{"", "example.com:443", 0, false},
		{"example.com:443", "", 0, false},
		{"example.com:443", "example.com:443", 1, false},
		{"example.com:443", "example.com:443", -1, false},
		{"example.com:443", "example.com:443", 0, true},
	} {
		f.Add(seed.target, seed.host, seed.contentLength, seed.transferEncoded)
	}
	f.Fuzz(func(t *testing.T, target, host string, contentLength int64, transferEncoded bool) {
		r := &http.Request{Method: http.MethodConnect, RequestURI: target, Host: host, ContentLength: contentLength}
		if transferEncoded {
			r.TransferEncoding = []string{"chunked"}
		}
		got, err := validateCONNECT(r)
		if contentLength != 0 || transferEncoded {
			if err == nil {
				t.Fatal("accepted CONNECT with forbidden body framing")
			}
			return
		}
		// The validator must agree with the authority parser for each input,
		// reject mismatches, and preserve canonical equivalents and IPv6.
		want, targetErr := hostport.Parse(target, "")
		hostAuthority, hostErr := hostport.Parse(host, "")
		if targetErr != nil || hostErr != nil || want != hostAuthority {
			if err == nil {
				t.Fatal("accepted invalid or mismatched CONNECT authorities")
			}
			return
		}
		if err != nil || got != want {
			t.Fatalf("matching CONNECT authorities: got %v, %v (want %v)", got, err, want)
		}
	})
}

func TestRefusalClosesConnectionBeforeSecondCONNECT(t *testing.T) {
	p, addr := start(t, config.Config{MaxConnections: 2, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second})
	conn := dialLifecycle(t, addr)
	reader := bufio.NewReader(conn)
	_, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: other.example:443\r\n\r\nCONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || !response.Close {
		t.Fatalf("first response = status %d, Close %v", response.StatusCode, response.Close)
	}
	_ = response.Body.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if second, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect}); err == nil {
		second.Body.Close()
		t.Fatalf("unexpected second response: %s", second.Status)
	}
	shutdown(t, p)
}

func TestGeneralOPTIONSClosesBeforeSubsequentCONNECT(t *testing.T) {
	_, addr := start(t, config.Config{MaxConnections: 2, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second})
	conn := dialLifecycle(t, addr)
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: proxy.invalid\r\n\r\nCONNECT registry.invalid:443 HTTP/1.1\r\nHost: other.invalid:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodOptions})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed || !resp.Close || string(body) != "method not allowed\n" {
		t.Fatalf("OPTIONS status=%d close=%v body=%q err=%v", resp.StatusCode, resp.Close, body, err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("connection after OPTIONS: %v, want EOF without a CONNECT response", err)
	}
}

func TestProxyHelperReportsLifecycleErrors(t *testing.T) {
	const modeEnv = "OCI_ACCELERATOR_PROXY_HELPER_TEST"
	switch os.Getenv(modeEnv) {
	case "serve":
		p := New(config.Config{MaxConnections: 1}, nil, nil, nil)
		done := serveProxy(t, p, failedAcceptListener{listen(t)})
		waitFor(t, done, "injected accept failure")
		return // cleanup must fail this subprocess on Serve's error
	case "shutdown":
		// Keep a pre-header connection open through proxy cleanup. Register
		// client cleanup first so it runs after the helper's shutdown check.
		var conn net.Conn
		t.Cleanup(func() {
			if conn != nil {
				conn.Close()
			}
		})
		p, addr := start(t, config.Config{MaxConnections: 1, ResponseHeaderTimeout: time.Minute})
		var err error
		conn, err = net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		eventuallyLifecycle(t, time.Second, func() bool { return lifecycleConnCount(p) == 1 }, "accepted pre-header connection")
		return // cleanup must fail this subprocess on Shutdown's deadline
	}
	// Use subprocesses to verify the helper actually fails testing.T, rather
	// than mocking a second reporting API that ordinary tests do not use.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, want string }{
		{"serve", "Serve: injected accept failure"},
		{"shutdown", "Shutdown: context deadline exceeded"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProxyHelperReportsLifecycleErrors$")
			cmd.Env = append(os.Environ(), modeEnv+"="+tc.mode)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), tc.want) {
				t.Fatalf("helper subprocess: err=%v output=%s, want test failure containing %q", err, output, tc.want)
			}
		})
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

type failedAcceptListener struct{ net.Listener }

func (l failedAcceptListener) Accept() (net.Conn, error) {
	return nil, errors.New("injected accept failure")
}
