package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
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
