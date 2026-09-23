package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
)

func TestCADownloadBootstrapsRegistryTrust(t *testing.T) {
	var upstreamRequests atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.URL.Path != "/ca.crt" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "registry response, not the proxy CA")
	}))
	defer origin.Close()
	p, addr, client := interceptFixture(t, origin, nil, false)
	want, err := os.ReadFile(p.signer.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	// Changing disk state must not change the certificate of the active signer.
	if err := os.WriteFile(p.signer.CertificatePath(), []byte("not the active CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	direct := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := direct.Get("http://" + addr.Host + "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(certificate, want) {
		t.Fatalf("download: status=%d matches=%v err=%v", resp.StatusCode, bytes.Equal(certificate, want), err)
	}
	if upstreamRequests.Load() != 0 {
		t.Fatal("certificate download reached the registry")
	}
	block, rest := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("download must contain exactly one public certificate PEM block")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !root.IsCA || root.CheckSignatureFrom(root) != nil {
		t.Fatal("download is not a self-signed CA")
	}
	fingerprint := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(fingerprint[:]) != p.signer.Fingerprint() {
		t.Fatal("download fingerprint differs from the startup fingerprint")
	}
	// Start with an empty trust store, then trust only the downloaded root.
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(root)
	resp, err = client.Get(origin.URL + "/ca.crt")
	if err != nil {
		t.Fatalf("registry TLS using downloaded CA: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "registry response, not the proxy CA" || upstreamRequests.Load() != 1 {
		t.Fatalf("registry response: status=%d body=%q requests=%d err=%v", resp.StatusCode, body, upstreamRequests.Load(), err)
	}
}

func TestCADownloadClosesBeforeCONNECT(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	defer origin.Close()
	p, addr, _ := interceptFixture(t, origin, nil, false)
	want, err := os.ReadFile(p.signer.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			conn := dialLifecycle(t, addr.Host)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			reader := bufio.NewReader(conn)
			request := method + " /ca.crt HTTP/1.1\r\nHost: proxy.invalid\r\n\r\n" +
				"CONNECT registry.invalid:443 HTTP/1.1\r\nHost: other.invalid:443\r\n\r\n"
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(reader, &http.Request{Method: method})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !resp.Close || resp.ContentLength != int64(len(want)) {
				t.Fatalf("download: status=%d close=%v length=%d err=%v", resp.StatusCode, resp.Close, resp.ContentLength, err)
			}
			for name, want := range map[string]string{
				"Content-Type":           "application/x-pem-file",
				"Content-Disposition":    `attachment; filename="ca.crt"`,
				"Cache-Control":          "no-store",
				"X-Content-Type-Options": "nosniff",
			} {
				if got := resp.Header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if method == http.MethodGet && !bytes.Equal(body, want) || method == http.MethodHead && len(body) != 0 {
				t.Fatalf("unexpected %s body (%d bytes)", method, len(body))
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatalf("connection after %s: %v, want EOF without a CONNECT response", method, err)
			}
		})
	}
}

func TestCADownloadRefusals(t *testing.T) {
	// A nil signer must fail closed. Invalid requests must be refused before
	// looking up the signer, and never disclose certificate or key material.
	_, addr := start(t, config.Config{MaxConnections: 2, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second})
	for _, tc := range []struct {
		name, method, target, headers, body string
		status                              int
		allow                               string
	}{
		{name: "no signer", method: "GET", target: "/ca.crt", status: 503},
		{name: "wrong method", method: "POST", target: "/ca.crt", status: 405, allow: "GET, HEAD"},
		{name: "body", method: "GET", target: "/ca.crt", headers: "Content-Length: 1\r\n", body: "x", status: 400},
		{name: "chunked", method: "GET", target: "/ca.crt", headers: "Transfer-Encoding: chunked\r\n", body: "0\r\n\r\n", status: 400},
		{name: "upgrade", method: "GET", target: "/ca.crt", headers: "Upgrade: websocket\r\nConnection: upgrade\r\n", status: 400},
		{name: "absolute URL", method: "GET", target: "http://registry.invalid/ca.crt", status: 405, allow: "CONNECT"},
		{name: "escaped path", method: "GET", target: "/ca%2ecrt", status: 405, allow: "CONNECT"},
		{name: "query", method: "GET", target: "/ca.crt?download=1", status: 405, allow: "CONNECT"},
		{name: "private key", method: "GET", target: "/ca.key", status: 405, allow: "CONNECT"},
		{name: "CONNECT unchanged", method: "CONNECT", target: "/ca.crt", status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialLifecycle(t, addr)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			reader := bufio.NewReader(conn)
			if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: proxy.invalid\r\n%s\r\n%s", tc.method, tc.target, tc.headers, tc.body); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(reader, &http.Request{Method: tc.method})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != tc.status || !resp.Close || resp.Header.Get("Allow") != tc.allow || bytes.Contains(body, []byte("-----BEGIN")) {
				t.Fatalf("refusal: status=%d close=%v allow=%q body=%q err=%v", resp.StatusCode, resp.Close, resp.Header.Get("Allow"), body, err)
			}
		})
	}
}
