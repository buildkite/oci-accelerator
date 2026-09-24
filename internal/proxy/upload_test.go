package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteBodiesStreamUpstream(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%t", method, chunked), func(t *testing.T) {
				first := make(chan struct{})
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					prefix := make([]byte, 3)
					if _, err := io.ReadFull(r.Body, prefix); err != nil {
						t.Error(err)
						return
					}
					close(first)
					rest, err := io.ReadAll(r.Body)
					if err != nil || string(prefix)+string(rest) != "abcDEFG" || r.Method != method || r.RequestURI != "/custom%2fupload?state=a%2Bb&state=x;y" {
						t.Errorf("upstream = %s %s %q %q, %v", r.Method, r.RequestURI, prefix, rest, err)
					}
					if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Content-Range") != "0-6" {
						t.Errorf("lost upload headers: %v", r.Header)
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, "accepted")
				}))
				defer origin.Close()
				_, addr, client := interceptFixture(t, origin, nil, true)
				conn, reader := uploadConnection(t, addr, client, origin.URL)
				framing, prefix, suffix := "Content-Length: 7", "abc", "DEFG"
				if chunked {
					framing, prefix, suffix = "Transfer-Encoding: chunked", "3\r\nabc\r\n", "4\r\nDEFG\r\n0\r\n\r\n"
				}
				_, err := fmt.Fprintf(conn, "%s /custom%%2fupload?state=a%%2Bb&state=x;y HTTP/1.1\r\nHost: %s\r\n%s\r\nAuthorization: Bearer fixture\r\nContent-Range: 0-6\r\n\r\n%s", method, strings.TrimPrefix(origin.URL, "https://"), framing, prefix)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-first:
				case <-time.After(time.Second):
					t.Fatal("proxy buffered the upload instead of forwarding its first bytes")
				}
				_, _ = io.WriteString(conn, suffix)
				resp, err := http.ReadResponse(reader, &http.Request{Method: method})
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusCreated || string(body) != "accepted" {
					t.Fatalf("response = %d %q, %v", resp.StatusCode, body, err)
				}
			})
		}
	}
}

func TestUploadEarlyRejection(t *testing.T) {
	for _, expect := range []bool{false, true} {
		t.Run(fmt.Sprintf("expect=%t", expect), func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Connection", "close")
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",scope="repository:team/image:push"`)
				http.Error(w, "push denied", http.StatusUnauthorized)
			}))
			defer origin.Close()
			_, addr, client := interceptFixture(t, origin, nil, true)
			conn, reader := uploadConnection(t, addr, client, origin.URL)
			extra := ""
			if expect {
				extra = "Expect: 100-continue\r\n"
			}
			_, _ = fmt.Fprintf(conn, "PUT /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: 1000000\r\n%s\r\n", strings.TrimPrefix(origin.URL, "https://"), extra)
			for {
				resp, err := http.ReadResponse(reader, &http.Request{Method: "PUT"})
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusContinue {
					continue
				}
				if err != nil || resp.StatusCode != http.StatusUnauthorized || string(body) != "push denied\n" || resp.Header.Get("WWW-Authenticate") == "" || calls.Load() != 1 {
					t.Fatalf("rejection = %d %q, %v, calls=%d", resp.StatusCode, body, err, calls.Load())
				}
				break
			}
		})
	}
}

func TestUploadStopsOnStallDisconnectAndShutdown(t *testing.T) {
	for _, stop := range []string{"stall", "disconnect", "shutdown"} {
		t.Run(stop, func(t *testing.T) {
			started, ended := make(chan struct{}), make(chan error, 1)
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				one := make([]byte, 1)
				_, err := io.ReadFull(r.Body, one)
				if err != nil || string(one) != "x" {
					t.Errorf("initial upload byte = %q, %v", one, err)
				}
				close(started)
				_, err = io.Copy(io.Discard, r.Body)
				ended <- err
			}))
			defer origin.Close()
			p, addr, client := interceptFixture(t, origin, nil, true)
			conn, _ := uploadConnection(t, addr, client, origin.URL)
			_, _ = fmt.Fprintf(conn, "PATCH /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: 10000\r\n\r\nx", strings.TrimPrefix(origin.URL, "https://"))
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream never received upload")
			}
			switch stop {
			case "disconnect":
				conn.Close()
			case "shutdown":
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if err := p.Shutdown(ctx); err != context.DeadlineExceeded {
					t.Fatalf("Shutdown = %v", err)
				}
			}
			select {
			case err := <-ended:
				if err == nil {
					t.Fatal("upstream accepted incomplete upload")
				}
			case <-time.After(time.Second):
				t.Fatal("upstream upload did not stop")
			}
		})
	}
}

func TestUploadReadDeadlineRefreshes(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "01234567" {
			t.Errorf("upload = %q, %v", body, err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer origin.Close()
	p, addr, client := interceptFixture(t, origin, nil, true)
	p.cfg.IdleTimeout = 500 * time.Millisecond
	conn, reader := uploadConnection(t, addr, client, origin.URL)
	_, _ = fmt.Fprintf(conn, "PUT /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: 8\r\n\r\n", strings.TrimPrefix(origin.URL, "https://"))
	for _, b := range "01234567" {
		_, _ = fmt.Fprintf(conn, "%c", b)
		time.Sleep(100 * time.Millisecond)
	}
	resp, err := http.ReadResponse(reader, &http.Request{Method: "PUT"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("long but active upload = %d", resp.StatusCode)
	}
}

func TestUploadContinueAndKeepAlive(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "payload" {
			t.Errorf("upload = %q, %v", body, err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer origin.Close()
	_, addr, client := interceptFixture(t, origin, nil, true)
	conn, reader := uploadConnection(t, addr, client, origin.URL)
	for range 2 {
		_, _ = fmt.Fprintf(conn, "PUT /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: 7\r\nExpect: 100-continue\r\n\r\n", strings.TrimPrefix(origin.URL, "https://"))
		resp, err := http.ReadResponse(reader, &http.Request{Method: "PUT"})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusContinue {
			t.Fatalf("expected continue, got %d", resp.StatusCode)
		}
		_, _ = io.WriteString(conn, "payload")
		resp, err = http.ReadResponse(reader, &http.Request{Method: "PUT"})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected created, got %d", resp.StatusCode)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestWriteFramingRejections(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	_, addr, client := interceptFixture(t, origin, nil, true)
	for _, tc := range []struct {
		method, headers string
		status          int
	}{
		{"PUT", "Content-Length: 1\r\nContent-Length: 2\r\n", http.StatusBadRequest},
		{"PUT", "Content-Length: -1\r\n", http.StatusBadRequest},
		{"PUT", "Transfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n", http.StatusNotImplemented},
		{"GET", "Content-Length: 1\r\n", http.StatusBadRequest},
		{"PUT", "Connection: upgrade\r\nUpgrade: websocket\r\n", http.StatusBadRequest},
	} {
		conn, reader := uploadConnection(t, addr, client, origin.URL)
		_, _ = fmt.Fprintf(conn, "%s /upload HTTP/1.1\r\nHost: %s\r\n%s\r\nx", tc.method, strings.TrimPrefix(origin.URL, "https://"), tc.headers)
		resp, err := http.ReadResponse(reader, &http.Request{Method: tc.method})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %q = %d", tc.method, tc.headers, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached upstream: %d", calls.Load())
	}
}

func uploadConnection(t *testing.T, addr proxyAddress, client *http.Client, origin string) (net.Conn, *bufio.Reader) {
	t.Helper()
	host := strings.TrimPrefix(origin, "https://")
	raw, _ := connectLifecycle(t, addr.Host, host, nil)
	config := client.Transport.(*http.Transport).TLSClientConfig.Clone()
	config.ServerName, _, _ = net.SplitHostPort(host)
	conn := tls.Client(raw, config)
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	return conn, bufio.NewReader(conn)
}
