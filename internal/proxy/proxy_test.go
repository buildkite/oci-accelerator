package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/config"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

func TestTunnelPreservesBufferedPayloadAndHalfClose(t *testing.T) {
	origin := listen(t)
	got := make(chan string, 1)
	go func() {
		conn, _ := origin.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		body, _ := io.ReadAll(conn)
		got <- string(body)
		_, _ = conn.Write([]byte("reply:" + string(body)))
	}()

	p, proxyAddr := start(t, config.Config{MaxConnections: 4, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second})
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	target := origin.Addr().String()
	_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\npayload")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v, %#v", err, response)
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	reply, _ := io.ReadAll(response.Body)
	if string(reply) != "reply:payload" {
		t.Fatalf("reply = %q", reply)
	}
	if body := <-got; body != "payload" {
		t.Fatalf("origin got %q", body)
	}
	shutdown(t, p)
}

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

// TestConcurrentCONNECTSniffingIsRaceFree drives many simultaneous CONNECT
// connections so the -race detector observes trackedConn.Read writing the sniff
// state (rawOK/checked) and ServeHTTP reading rawOK across all of them at once.
//
// For a single connection those two accesses happen on the same net/http
// connection goroutine (the head is read, then ServeHTTP is dispatched), so they
// are ordered by program order and no lock is required. This test does not try
// to manufacture a cross-goroutine race that cannot occur today; it is a guard
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
		{"non-connect method deferred", "GET / HTTP/1.1\r\nHost: example.com", true},
		{"wrong word count deferred", "CONNECT not a target HTTP/1.1\r\nHost: example.com:443", true},
		{"irregular spacing deferred to net/http", "CONNECT  example.com:443 HTTP/1.1\r\nHost: other.example:443", true},
		{"malformed first line deferred", "garbage line without three parts", true},
		{"empty first line deferred", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawCONNECTAcceptable([]byte(tc.head)); got != tc.want {
				t.Fatalf("rawCONNECTAcceptable(%q) = %v, want %v", tc.head, got, tc.want)
			}
		})
	}
}

func FuzzValidateCONNECT(f *testing.F) {
	for _, seed := range []string{"example.com:443", "[::1]:5000", "example.com", "user@host:1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, target string) {
		r := &http.Request{Method: http.MethodConnect, RequestURI: target, Host: target}
		a, err := validateCONNECT(r)
		if err == nil && !strings.Contains(net.JoinHostPort(a.Host(), a.Port()), ":") {
			t.Fatal("invalid canonical result")
		}
	})
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

func start(t *testing.T, cfg config.Config) (*Proxy, string) {
	t.Helper()
	l := listen(t)
	p := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	go func() { _ = p.Serve(l) }()
	t.Cleanup(func() { shutdown(t, p) })
	return p, l.Addr().String()
}

func shutdown(t *testing.T, p *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
