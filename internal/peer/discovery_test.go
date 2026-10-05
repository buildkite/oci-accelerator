package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

func TestDiscoveryThreeNodesConvergeAndContactLearnedPeers(t *testing.T) {
	authority, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, clientTLS, err := authority.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}

	type node struct {
		book      *Book
		discovery *Discovery
		server    *httptest.Server
		counts    map[string]int
		mu        sync.Mutex
	}
	nodes := make([]*node, 3)
	for i := range nodes {
		n := &node{counts: make(map[string]int)}
		listener := httptest.NewUnstartedServer(nil)
		address := listener.Listener.Addr().String()
		n.book, err = OpenBook(bookTestDir(t), address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.book.Close() })
		n.discovery = NewDiscovery(n.book, NewClient(clientTLS.Clone()), slog.New(slog.NewTextHandler(io.Discard, nil)))
		n.discovery.interval = 10 * time.Millisecond
		n.discovery.maxBackoff = 40 * time.Millisecond
		handler := n.discovery.Handler()
		listener.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, readErr := io.ReadAll(r.Body)
			if readErr == nil {
				var message Message
				if json.Unmarshal(raw, &message) == nil {
					n.mu.Lock()
					n.counts[message.Address]++
					n.mu.Unlock()
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			handler.ServeHTTP(w, r)
		})
		listener.TLS = serverTLS.Clone()
		listener.StartTLS()
		t.Cleanup(listener.Close)
		n.server = listener
		nodes[i] = n
	}
	addresses := []string{nodes[0].book.self, nodes[1].book.self, nodes[2].book.self}
	if _, err := nodes[0].book.Merge(Message{Address: addresses[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes[1].book.Merge(Message{Address: addresses[2]}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var runs sync.WaitGroup
	for _, n := range nodes {
		runs.Add(1)
		go func() { defer runs.Done(); n.discovery.Run(ctx) }()
	}
	t.Cleanup(func() { cancel(); runs.Wait() })
	want := [][]string{{addresses[1], addresses[2]}, {addresses[0], addresses[2]}, {addresses[0], addresses[1]}}
	for i := range want {
		sort.Strings(want[i])
	}
	converged := func() bool {
		for i, n := range nodes {
			if !reflect.DeepEqual(n.book.Snapshot().Peers, want[i]) {
				return false
			}
		}
		return true
	}
	eventually(t, 3*time.Second, converged)
	// A initially knew only B. A request observed by C proves A scheduled the
	// newly learned address rather than merely copying C into its book.
	eventually(t, 3*time.Second, func() bool {
		nodes[2].mu.Lock()
		defer nodes[2].mu.Unlock()
		return nodes[2].counts[addresses[0]] > 0
	})
	cancel()
	runs.Wait()
}

func TestDiscoveryRetriesFailureAndResetsAfterRecovery(t *testing.T) {
	book, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if _, err := book.Merge(Message{Address: "peer.test:2"}); err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	failing.Store(true)
	var attempts atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		if failing.Load() {
			return nil, context.DeadlineExceeded
		}
		body, _ := json.Marshal(Message{Address: "peer.test:2", Peers: []string{"learned.test:3"}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	d := NewDiscovery(book, &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.interval, d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, time.Second, func() bool { return attempts.Load() >= 3 })
	failing.Store(false)
	eventually(t, time.Second, func() bool {
		return reflect.DeepEqual(book.Snapshot().Peers, []string{"learned.test:3", "peer.test:2"})
	})
	before := attempts.Load()
	eventually(t, time.Second, func() bool { return attempts.Load() >= before+2 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not join after cancellation")
	}
}

func TestRetryDelayExponentialCap(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{{0, 10}, {1, 10}, {2, 20}, {3, 35}, {10, 35}} {
		if got := retryDelay(10, 35, tc.failures); got != tc.want {
			t.Errorf("failures %d: got %v, want %v", tc.failures, got, tc.want)
		}
	}
}

func TestDiscoveryLimitsConcurrencyAndCancellationJoinsRequests(t *testing.T) {
	book, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for i := 0; i < 8; i++ {
		_, _ = book.Merge(Message{Address: "peer" + string(rune('a'+i)) + ".test:1"})
	}
	var active, maximum, finished atomic.Int32
	started := make(chan struct{}, 8)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		started <- struct{}{}
		defer active.Add(-1)
		defer finished.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	d := NewDiscovery(book, &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < exchangeConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("requests did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run returned without joining requests")
	}
	if maximum.Load() != exchangeConcurrency || finished.Load() != exchangeConcurrency {
		t.Fatalf("maximum=%d finished=%d, want %d", maximum.Load(), finished.Load(), exchangeConcurrency)
	}
}

func TestDiscoveryAttemptsUntriedPeersBeforeRetryingDuePeers(t *testing.T) {
	book, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for _, host := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if _, err := book.Merge(Message{Address: host + ".test:1"}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 8)
	release := make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case started <- r.URL.Host:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		// Hold the second batch open so goroutine scheduling cannot reorder
		// a legitimate subsequent retry ahead of its recorded first attempts.
		if r.URL.Host >= "e.test:1" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil, context.DeadlineExceeded
	})
	d := NewDiscovery(book, &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Every completed request is due again by the next scheduling pass. A
	// lexical-first scheduler would repeatedly retry early addresses instead
	// of giving untried peers their first connection attempt.
	d.interval, d.maxBackoff = time.Nanosecond, time.Nanosecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	seen := make(map[string]bool)
	for i := 0; i < 8; i++ {
		select {
		case address := <-started:
			if seen[address] {
				t.Fatalf("retried %s before attempting every peer", address)
			}
			seen[address] = true
		case <-time.After(time.Second):
			t.Fatal("peer attempt did not start")
		}
		if i == 3 {
			close(release)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}
