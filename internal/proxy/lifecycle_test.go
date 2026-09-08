package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/config"
)

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

func TestUpstreamFINReachesClientWhileClientWriteRemainsOpen(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: 800 * time.Millisecond})
	client, reader := connectLifecycle(t, addr, origin.Addr().String(), nil)
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	if _, err := upstream.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := upstream.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read before idle timeout: %v", err)
	}
	if string(got) != "reply" {
		t.Fatalf("reply = %q", got)
	}
	shutdown(t, p)
}

func TestOneWayTrafficWithBufferedCONNECTPayloadStaysActive(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	idle := 150 * time.Millisecond
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: idle})
	client, _ := connectLifecycle(t, addr, origin.Addr().String(), []byte("initial"))
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(upstream)
		got <- b
	}()
	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "initialxxxxxxxx" {
			t.Fatalf("origin received %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("origin did not receive EOF")
	}
	shutdown(t, p)
}

func TestIdleTunnelExpiryClosesClientAndUpstream(t *testing.T) {
	origin := listen(t)
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := origin.Accept(); accepted <- c }()
	p, addr := start(t, config.Config{MaxConnections: 2, DialTimeout: time.Second, ResponseHeaderTimeout: time.Second, IdleTimeout: 120 * time.Millisecond})
	client, _ := connectLifecycle(t, addr, origin.Addr().String(), nil)
	upstream := <-accepted
	t.Cleanup(func() { upstream.Close() })
	assertLifecycleClosed(t, client, "client")
	assertLifecycleClosed(t, upstream, "upstream")
	shutdown(t, p)
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

type shutdownLogWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *shutdownLogWriter) Write(b []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(b), nil
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

func dialLifecycle(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
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
