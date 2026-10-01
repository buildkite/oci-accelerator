package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// TestConcurrentCONNECTSniffingIsRaceFree drives many simultaneous CONNECT
// connections so the -race detector observes trackedConn.Read writing the sniff
// state (rawOK/checked) and ServeHTTP reading rawOK across all of them at once.
//
// For a single connection those two accesses happen on the same net/http
// connection goroutine (the head is read, then ServeHTTP is dispatched), so they
// are ordered by program order and no lock is required. This test does not try
// to manufacture a cross-goroutine race that cannot occur today. It is a guard
// that would fail under -race if future changes ever moved the rawOK read or
// write onto a different goroutine without synchronization. The status
// assertions also confirm the sniff verdict still drives the correct refusal or
// acceptance.
func TestConcurrentCONNECTSniffingIsRaceFree(t *testing.T) {
	configured, _ := hostport.Parse("registry.invalid:443", "443")
	// start registers t.Cleanup(shutdown), so the proxy is drained automatically.
	_, addr := start(t, config.Config{
		Registries:            []hostport.HostPort{configured},
		MaxConnections:        64,
		DialTimeout:           time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		IdleTimeout:           2 * time.Second,
	})

	cases := []struct {
		request string
		status  int
	}{
		// Valid CONNECT to the configured registry: rawOK is true, and with no
		// signer the handler returns 503 after reading the sniff verdict.
		{"CONNECT registry.invalid:443 HTTP/1.1\r\nHost: registry.invalid:443\r\n\r\n", http.StatusServiceUnavailable},
		// Mismatched Host: the raw sniff sets rawOK false, so ServeHTTP refuses.
		{"CONNECT registry.invalid:443 HTTP/1.1\r\nHost: other.invalid:443\r\n\r\n", http.StatusBadRequest},
	}

	const perCase = 48
	var wg sync.WaitGroup
	errs := make(chan error, len(cases)*perCase)
	for _, tc := range cases {
		for i := 0; i < perCase; i++ {
			wg.Add(1)
			go func(request string, want int) {
				defer wg.Done()
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					errs <- fmt.Errorf("dial: %w", err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := io.WriteString(conn, request); err != nil {
					errs <- fmt.Errorf("write: %w", err)
					return
				}
				resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
				if err != nil {
					errs <- fmt.Errorf("read response: %w", err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != want {
					errs <- fmt.Errorf("status = %d, want %d", resp.StatusCode, want)
				}
			}(tc.request, tc.status)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestRawCONNECTAcceptable(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		want       bool
	}{
		{"valid matching authority", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443", true},
		{"valid bracketed ipv6 authority", "CONNECT [::1]:5000 HTTP/1.1\r\nHost: [::1]:5000", true},
		{"target host mismatch", "CONNECT example.com:443 HTTP/1.1\r\nHost: other.example:443", false},
		{"target port mismatch", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:8443", false},
		{"unparseable target in three-part line", "CONNECT @@@ HTTP/1.1\r\nHost: example.com:443", false},
		{"unparseable host", "CONNECT example.com:443 HTTP/1.1\r\nHost: @@@", false},
		{"missing host header", "CONNECT example.com:443 HTTP/1.1\r\nUser-Agent: x", false},
		{"duplicate host header", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nHost: example.com:443", false},
		{"non-connect method", "GET / HTTP/1.1\r\nHost: example.com", false},
		{"wrong word count", "CONNECT not a target HTTP/1.1\r\nHost: example.com:443", false},
		{"irregular spacing", "CONNECT  example.com:443 HTTP/1.1\r\nHost: other.example:443", false},
		{"malformed first line", "garbage line without three parts", false},
		{"empty first line", "", false},
		{"LF request line", "CONNECT example.com:443 HTTP/1.1\nHost: other.example:443", false},
		{"LF header line", "CONNECT example.com:443 HTTP/1.1\r\nX-Test: x\nHost: other.example:443", false},
		{"folded header", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\tother.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawCONNECTAcceptable([]byte(tc.head)); got != tc.want {
				t.Fatalf("rawCONNECTAcceptable(%q) = %v, want %v", tc.head, got, tc.want)
			}
		})
	}
}

func TestCONNECTRejectsAmbiguousWireSyntax(t *testing.T) {
	authority, _ := hostport.Parse("registry.invalid:443", "443")
	_, addr := start(t, config.Config{Registries: []hostport.HostPort{authority}, MaxConnections: 4, ResponseHeaderTimeout: time.Second})
	for _, head := range []string{
		"CONNECT registry.invalid:443 HTTP/1.1\nHost: other.invalid:443",
		"CONNECT registry.invalid:443 HTTP/1.1\nHost: registry.invalid:443",
		"CONNECT registry.invalid:443 HTTP/1.1\r\nX-Test: x\nHost: other.invalid:443",
		"CONNECT  registry.invalid:443 HTTP/1.1\r\nHost: other.invalid:443",
	} {
		t.Run(head, func(t *testing.T) {
			conn := dialLifecycle(t, addr)
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(conn, head+"\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !resp.Close {
				t.Fatalf("status=%d close=%v, want 400 and close", resp.StatusCode, resp.Close)
			}
		})
	}
}

func TestMaxConnectionsBoundsAcceptedMapBeforeHeaders(t *testing.T) {
	p, addr := start(t, config.Config{MaxConnections: 2, ResponseHeaderTimeout: 2 * time.Second, IdleTimeout: time.Second})
	first := dialLifecycle(t, addr)
	second := dialLifecycle(t, addr)
	eventuallyLifecycle(t, time.Second, func() bool { return lifecycleConnCount(p) == 2 }, "two accepted connections")
	third := dialLifecycle(t, addr)
	time.Sleep(100 * time.Millisecond)
	if got := lifecycleConnCount(p); got != 2 {
		t.Fatalf("accepted map has %d connections, want 2", got)
	}
	firstAddr := first.LocalAddr().String()
	thirdAddr := third.LocalAddr().String()
	first.Close()
	eventuallyLifecycle(t, time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		thirdAccepted := false
		for conn := range p.conns {
			remote := conn.RemoteAddr().String()
			if remote == firstAddr {
				return false
			}
			if remote == thirdAddr {
				thirdAccepted = true
			}
		}
		return len(p.conns) == 2 && thirdAccepted
	}, "queued connection accepted after first slot release")
	second.Close()
	third.Close()
	shutdown(t, p)
}
