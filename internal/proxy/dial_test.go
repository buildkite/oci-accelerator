package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/go-crew-kit/outbound"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestDialUpstreamDestinationPolicy(t *testing.T) {
	authority, _ := hostport.Parse("storage.test:5000", "443")
	otherPort, _ := hostport.Parse("storage.test:5001", "443")

	for _, tc := range []struct {
		name       string
		address    string
		registries []hostport.HostPort
		redirects  []hostport.HostPort
		allowed    bool
	}{
		{name: "unconfigured public", address: "8.8.8.8", allowed: true},
		{name: "unconfigured private", address: "127.0.0.1"},
		{name: "configured registry", address: "127.0.0.1", registries: []hostport.HostPort{authority}, allowed: true},
		{name: "redirect exception", address: "127.0.0.1", redirects: []hostport.HostPort{authority}, allowed: true},
		{name: "exception for another port", address: "127.0.0.1", redirects: []hostport.HostPort{otherPort}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{DialTimeout: time.Second, Registries: tc.registries, RedirectAllowlist: tc.redirects}
			p := New(cfg, nil, nil, nil)
			p.lookupNetIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
				if network != "ip" || host != "storage.test" {
					t.Fatalf("lookup = %s %s", network, host)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > cfg.DialTimeout {
					t.Fatal("configured dial timeout did not bound DNS")
				}

				return []netip.Addr{netip.MustParseAddr(tc.address)}, nil
			}
			p.dialIP = func(_ context.Context, network, address string) (net.Conn, error) {
				if !tc.allowed || network != "tcp" || address != net.JoinHostPort(tc.address, "5000") {
					t.Fatalf("unexpected dial: %s %s", network, address)
				}

				client, server := net.Pipe()
				t.Cleanup(func() { server.Close() })
				return client, nil
			}

			conn, err := p.dialUpstream(context.Background(), "tcp", "STORAGE.TEST.:5000")
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
			} else if conn != nil || outbound.Classify(err) != outbound.AddressDenied {
				t.Fatalf("forbidden answer: conn=%v err=%v", conn, err)
			}
		})
	}
}

func TestDialUpstreamRefusalLogsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		address    string
		lookupIP   func(context.Context, string, string) ([]netip.Addr, error)
		secret     string
		logMessage string
	}{
		{
			name:       "unparseable address",
			address:    "private invalid authority",
			secret:     "private invalid authority",
			logMessage: "reason=dial_address",
		},
		{
			name:    "resolution failure",
			address: "resolve-fail.test:443",
			lookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
				return nil, errors.New("private-dns-detail")
			},
			secret:     "private-dns-detail",
			logMessage: "reason=dial_resolve",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			p := New(config.Config{DialTimeout: 500 * time.Millisecond}, nil, logger, nil)
			if tc.lookupIP != nil {
				p.lookupNetIP = tc.lookupIP
			} else {
				p.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
					t.Fatal("invalid authority reached DNS")
					return nil, nil
				}
			}
			p.dialIP = func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("refused destination reached dialing")
				return nil, nil
			}

			conn, err := p.dialUpstream(context.Background(), "tcp", tc.address)
			if conn != nil {
				conn.Close()
				t.Fatal("dial unexpectedly succeeded")
			}
			if err == nil {
				t.Fatal("refused dial returned no error")
			}

			if !strings.Contains(logs.String(), tc.logMessage) {
				t.Errorf("logs %q missing reason %q", logs.String(), tc.logMessage)
			}
			if strings.Contains(logs.String(), tc.secret) {
				t.Errorf("logs contain private failure detail: %s", logs.String())
			}
		})
	}
}

func TestUpstreamKeepAliveReadDeadlineRefreshesForNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		var mu sync.Mutex
		var remotes []string
		secondStarted, releaseSecond := make(chan struct{}), make(chan struct{})
		origin := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := requests.Add(1)
			mu.Lock()
			remotes = append(remotes, r.RemoteAddr)
			mu.Unlock()
			if n == 1 {
				_, _ = io.WriteString(w, "first")
				return
			}
			if n == 2 {
				close(secondStarted)
			}
			select {
			case <-releaseSecond:
				_, _ = io.WriteString(w, "second")
			case <-r.Context().Done():
			}
		}))
		originTransport := origin.Client().Transport.(*http.Transport)
		authority, _ := hostport.Parse("example.com:80", "443")
		p := New(config.Config{Registries: []hostport.HostPort{authority}, DialTimeout: time.Minute, IdleTimeout: 800 * time.Millisecond}, nil, nil, nil)
		defer p.transport.CloseIdleConnections()
		p.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		p.dialIP = originTransport.DialContext
		client := &http.Client{Transport: p.transport}
		get := func(path string) (string, error) {
			resp, err := client.Get("http://example.com" + path)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			return string(body), err
		}
		if body, err := get("/v2/first"); err != nil || body != "first" {
			t.Fatalf("first GET: body=%q err=%v", body, err)
		}

		// Quiescence leaves the pool reader blocked with its original 800ms
		// deadline. The next request must refresh that already-pending read.
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		type result struct {
			body string
			err  error
		}
		resultCh := make(chan result, 1)
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		release := safeRelease(releaseSecond)
		defer func() {
			cancel()
			release()
			<-done
		}()
		go func() {
			defer close(done)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/v2/second", nil)
			resp, err := client.Do(req)
			if err != nil {
				resultCh <- result{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			resultCh <- result{body: string(body), err: err}
		}()
		select {
		case <-secondStarted:
		case res := <-resultCh:
			t.Fatalf("second GET ended before reaching origin: %+v", res)
		case <-time.After(time.Second):
			t.Fatal("second GET did not reach origin")
		}
		synctest.Wait()
		time.Sleep(400 * time.Millisecond)
		release()
		res := <-resultCh
		if res.err != nil || res.body != "second" {
			t.Errorf("second GET: body=%q err=%v", res.body, res.err)
		}
		mu.Lock()
		gotRemotes := append([]string(nil), remotes...)
		mu.Unlock()
		if requests.Load() != 2 || len(gotRemotes) != 2 || gotRemotes[0] != gotRemotes[1] {
			t.Errorf("expected exactly two GETs on one connection: requests=%d remotes=%v", requests.Load(), gotRemotes)
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
	p, _, client := interceptFixture(t, origin, nil, true)
	p.cfg.IdleTimeout = 150 * time.Millisecond

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
