package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/config"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

func TestDialUpstreamSharesRemainingTimeout(t *testing.T) {
	for _, first := range []string{"timeout", "refused", "canceled"} {
		t.Run(first, func(t *testing.T) {
			origin := listen(t)
			go func() {
				conn, err := origin.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.WriteString(conn, "healthy address")
			}()
			_, port, _ := net.SplitHostPort(origin.Addr().String())
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
				if len(attempts) != 2 || address != origin.Addr().String() {
					t.Fatalf("unexpected fallback dial: %s", address)
				}
				if ctx.Err() != nil || time.Until(until) <= 0 {
					t.Fatal("first attempt exhausted the fallback budget")
				}
				if deadline.Sub(until) > time.Millisecond {
					t.Fatal("last attempt did not receive the remaining budget")
				}
				return (&net.Dialer{}).DialContext(attempt, network, address)
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
