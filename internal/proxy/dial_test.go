package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestDialUpstreamSharesRemainingTimeout(t *testing.T) {
	for _, first := range []string{"timeout", "refused", "canceled"} {
		t.Run(first, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server, fallback := net.Pipe()
				defer server.Close()
				defer fallback.Close()
				go func() {
					defer server.Close()
					_, _ = io.WriteString(server, "healthy address")
				}()

				const port = "5000"
				authority, _ := hostport.Parse("storage.test:"+port, "443")
				p := New(config.Config{DialTimeout: time.Minute, IdleTimeout: time.Second, RedirectAllowlist: []hostport.HostPort{authority}}, nil, nil, nil)

				// The caller's shorter deadline, not DialTimeout, must govern all
				// attempts. DNS and each preceding attempt consume this same budget.
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				deadline, _ := ctx.Deadline()

				lookups := 0
				p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
					lookups++
					return []net.IPAddr{{IP: net.ParseIP("127.0.0.2")}, {IP: net.ParseIP("127.0.0.1")}}, nil
				}

				var attempts []context.Context
				p.dialIP = func(attempt context.Context, network, address string) (net.Conn, error) {
					attempts = append(attempts, attempt)
					until, ok := attempt.Deadline()
					if !ok || until.After(deadline) {
						t.Fatalf("attempt deadline %v exceeds caller deadline %v", until, deadline)
					}
					if len(attempts) == 1 {
						if address != net.JoinHostPort("127.0.0.2", port) {
							t.Fatalf("first dial did not use validated IP: %s", address)
						}
						if remaining := time.Until(until); remaining <= 0 || remaining > 500*time.Millisecond {
							t.Fatalf("first attempt did not reserve half the budget: %v", remaining)
						}
						switch first {
						case "timeout":
							<-attempt.Done()
							return nil, attempt.Err()
						case "canceled":
							cancel()
							return nil, attempt.Err()
						default:
							return nil, errors.New("connection refused")
						}
					}
					if len(attempts) != 2 || address != net.JoinHostPort("127.0.0.1", port) {
						t.Fatalf("unexpected fallback dial: %s", address)
					}
					if ctx.Err() != nil || time.Until(until) <= 0 {
						t.Fatal("first attempt exhausted the fallback budget")
					}
					if deadline.Sub(until) > time.Millisecond {
						t.Fatal("last attempt did not receive the remaining budget")
					}
					return fallback, nil
				}

				conn, err := p.dialUpstream(ctx, "tcp", authority.String())
				if first == "canceled" {
					if conn != nil {
						conn.Close()
						t.Fatal("canceled dial returned a connection")
					}
					if !errors.Is(err, errDialConnect) || len(attempts) != 1 {
						t.Fatalf("canceled dial = %v, attempts = %d", err, len(attempts))
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					if body, err := io.ReadAll(conn); err != nil || string(body) != "healthy address" {
						t.Fatalf("fallback body = %q, %v", body, err)
					}
					if len(attempts) != 2 {
						t.Fatalf("dial attempts = %d, want 2", len(attempts))
					}
				}
				if lookups != 1 {
					t.Fatalf("DNS lookups = %d, want 1", lookups)
				}
				for _, attempt := range attempts {
					if attempt.Err() == nil {
						t.Fatal("attempt context was not released")
					}
				}
			})
		})
	}
}

func TestDialUpstreamValidatesWholeAnswerBeforeAnyAttempt(t *testing.T) {
	p := New(config.Config{DialTimeout: time.Second}, nil, nil, nil)
	p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	p.dialIP = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial attempted before the entire DNS answer was validated")
		return nil, nil
	}
	conn, err := p.dialUpstream(context.Background(), "tcp", "storage.test:443")
	if conn != nil || !errors.Is(err, errDialForbidden) {
		t.Fatalf("mixed DNS answer: conn = %v, err = %v", conn, err)
	}
}

func TestAddressPolicyAndRebinding(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1", "fec0::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "224.0.0.1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(ip)) {
			t.Errorf("blocked %s", ip)
		}
	}
	p := New(config.Config{DialTimeout: time.Second}, nil, nil, nil)
	l := listen(t)
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	calls := 0
	p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port); err == nil {
		c.Close()
		t.Fatal("mixed answer accepted")
	}
	a, _ := hostport.Parse("storage.test:"+port, "443")
	p.destinations[a.String()] = struct{}{}
	calls = 0
	p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls > 1 {
			t.Error("DNS re-resolved after validation")
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	delete(p.destinations, a.String())
	calls = 0
	if c, err := p.dialUpstream(context.Background(), "tcp", "storage.test:"+port); err == nil {
		c.Close()
		t.Fatal("private DNS answer accepted")
	}
}

func TestDialUpstreamRefusalReasonsAreDistinctAndLoggedWhileRefusingClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		address    string
		lookupIP   func(context.Context, string) ([]net.IPAddr, error)
		allowlist  bool
		reason     error
		logMessage string
	}{
		{
			name:       "unparseable address",
			address:    "not a valid authority",
			reason:     errDialAddress,
			logMessage: "unparseable upstream address",
		},
		{
			name:       "resolution failure",
			address:    "resolve-fail.test:443",
			lookupIP:   func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") },
			reason:     errDialResolve,
			logMessage: "upstream DNS resolution failed",
		},
		{
			name:       "empty answer",
			address:    "empty-answer.test:443",
			lookupIP:   func(context.Context, string) ([]net.IPAddr, error) { return nil, nil },
			reason:     errDialResolve,
			logMessage: "upstream DNS resolution failed",
		},
		{
			name:    "forbidden private address",
			address: "private.test:443",
			lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			},
			reason:     errDialForbidden,
			logMessage: "upstream address not permitted",
		},
		{
			name: "connection refused",
			lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			},
			allowlist:  true,
			reason:     errDialConnect,
			logMessage: "upstream connection failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			p := New(config.Config{DialTimeout: 500 * time.Millisecond}, nil, logger, nil)
			address := tc.address
			if tc.name == "connection refused" {
				// A closed listener guarantees a connection failure at a real
				// loopback authority. Allowlisting it lets the dial reach the
				// connect branch instead of being refused as a private address.
				l := listen(t)
				_, port, _ := net.SplitHostPort(l.Addr().String())
				l.Close()
				address = "127.0.0.1:" + port
			}
			if tc.lookupIP != nil {
				p.lookupIP = tc.lookupIP
			}
			if tc.allowlist {
				a, _ := hostport.Parse(address, "443")
				p.destinations[a.String()] = struct{}{}
			}
			conn, err := p.dialUpstream(context.Background(), "tcp", address)
			if conn != nil {
				conn.Close()
				t.Fatal("dial unexpectedly succeeded")
			}
			if errors.Is(err, errRedirect) {
				t.Fatalf("dial failure classified as a redirect: %v", err)
			}
			if !errors.Is(err, tc.reason) {
				t.Fatalf("err = %v, want reason %v", err, tc.reason)
			}
			if !strings.Contains(logs.String(), tc.logMessage) {
				t.Errorf("logs %q missing reason %q", logs.String(), tc.logMessage)
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
		p.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
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
