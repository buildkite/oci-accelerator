package peering

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestAvailabilityRelaysOriginAndStopsWhenBookHasSeenRevision(t *testing.T) {
	nodes, network := availabilityFleet(t, 4)
	a, b, c, d := nodes[0], nodes[1], nodes[2], nodes[3]
	learnAvailabilityPeers(t, a, b)
	learnAvailabilityPeers(t, b, a, c)
	learnAvailabilityPeers(t, c, a, b)
	now := time.Now()
	a.index.syncLocal(a.self, a.session, []string{testKey(1)}, false, now)
	original := a.index.batch(b.self, now)[0]
	relayOnce(a, now)
	relayOnce(b, now)
	if got := c.Owners(testKey(1)); !slices.Equal(got, []string{a.self}) {
		t.Fatalf("C owners = %v, want original owner %s", got, a.self)
	}
	forwarded := c.index.batch(d.self, now)
	if len(forwarded) != 1 || fieldsOf(forwarded[0]) != fieldsOf(original) {
		t.Fatalf("relay changed origin or lease: %+v", forwarded)
	}
	if !slices.Equal(forwarded[0].Seen, []string{a.self, b.self, c.self}) {
		t.Fatalf("relayed seen addresses = %v", forwarded[0].Seen)
	}
	if network.total() != 2 {
		t.Fatalf("A→B→C took %d requests", network.total())
	}
	for range 3 {
		for _, node := range nodes[:3] {
			relayOnce(node, now)
		}
	}
	if network.total() != 2 {
		t.Fatalf("fully seen revision kept circulating: %d requests", network.total())
	}

	// A new address in the book receives the retained revision, even though
	// propagation had stopped among the old peers.
	learnAvailabilityPeers(t, c, d)
	relayOnce(c, now)
	if network.total() != 3 || !slices.Equal(d.Owners(testKey(1)), []string{a.self}) {
		t.Fatalf("new peer did not learn retained claim: requests=%d owners=%v", network.total(), d.Owners(testKey(1)))
	}

	// A withdrawal has fresh seen state and traverses the same path.
	a.index.syncLocal(a.self, a.session, nil, false, now)
	for _, node := range nodes[:3] {
		relayOnce(node, now)
	}
	if network.total() != 6 {
		t.Fatalf("withdrawal delivery requests = %d, want 6 total", network.total())
	}
	for _, node := range nodes {
		if got := node.Owners(testKey(1)); len(got) != 0 {
			t.Fatalf("%s retained withdrawn owner %v", node.self, got)
		}
	}
	if _, err := c.index.merge([]Claim{original}, c.self, a.self, now); err != nil {
		t.Fatal(err)
	}
	if got := c.Owners(testKey(1)); len(got) != 0 {
		t.Fatalf("replayed original resurrected claim: %v", got)
	}
}

func TestAvailabilityFailureBackoffDoesNotAcknowledgeDroppedClaims(t *testing.T) {
	nodes, network := availabilityFleet(t, 2)
	a, b := nodes[0], nodes[1]
	learnAvailabilityPeers(t, a, b)
	b.index.maxRecords = 0
	now := time.Now()
	a.index.syncLocal(a.self, a.session, []string{testKey(1)}, false, now)
	known, cursor := make(map[string]*peerSchedule), 0
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, now)
	if network.total() != 1 || len(b.Owners(testKey(1))) != 0 || len(a.index.batch(b.self, now)) != 1 {
		t.Fatal("failed admission was acknowledged or stored")
	}
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, now)
	if network.total() != 1 {
		t.Fatal("retry ignored backoff")
	}
	b.index.maxRecords = 2
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, known[b.self].next.Add(time.Millisecond))
	if network.total() != 2 || !slices.Equal(b.Owners(testKey(1)), []string{a.self}) || known[b.self].failures != 0 {
		t.Fatal("retry did not recover and record owner")
	}
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, time.Now())
	if network.total() != 2 {
		t.Fatal("successful retry was not acknowledged")
	}
}

func TestAvailabilityRelayBoundsFanoutAndVisitsLaterPeers(t *testing.T) {
	nodes, network := availabilityFleet(t, 7)
	a := nodes[0]
	learnAvailabilityPeers(t, a, nodes[1:]...)
	now := time.Now()
	a.index.syncLocal(a.self, a.session, []string{testKey(1)}, false, now)
	known, cursor := make(map[string]*peerSchedule), 0
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, now)
	if network.total() != availabilityFanout {
		t.Fatalf("first round sent %d requests", network.total())
	}
	a.relay(context.Background(), a.book.Snapshot().Peers, known, &cursor, now)
	if network.total() != 6 {
		t.Fatalf("later peers were starved: %d requests", network.total())
	}
	for _, node := range nodes[1:] {
		if got := node.Owners(testKey(1)); !slices.Equal(got, []string{a.self}) {
			t.Fatalf("%s owners = %v", node.self, got)
		}
	}
}

func TestAvailabilityRunInventoriesRestoredCacheAndWithdrawsEviction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	limits := cache.Limits{MaxBytes: 1 << 20, MaxEntries: 1, MaxFills: 1, FillTimeout: time.Minute}
	store, err := cache.OpenWithLimits(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	putAvailabilityBody(t, store, testKey(1), "restored bytes")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = cache.OpenWithLimits(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	book, err := OpenBook(bookTestDir(t), testSelf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	a := NewAvailability(book, store, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, time.Second, func() bool { return slices.Equal(a.Owners(testKey(1)), []string{testSelf}) })
	putAvailabilityBody(t, store, testKey(2), "new bytes")
	eventually(t, time.Second, func() bool {
		return len(a.Owners(testKey(1))) == 0 && slices.Equal(a.Owners(testKey(2)), []string{testSelf})
	})
	if store.Stats().Evictions != 1 {
		t.Fatalf("evictions = %d", store.Stats().Evictions)
	}
}

type availabilityNetwork struct {
	mu       sync.Mutex
	requests int
	handlers map[string]http.Handler
}

func availabilityFleet(t *testing.T, size int) ([]*Availability, *availabilityNetwork) {
	t.Helper()
	network := &availabilityNetwork{handlers: make(map[string]http.Handler)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		network.mu.Lock()
		network.requests++
		handler := network.handlers[r.URL.Host]
		network.mu.Unlock()
		if handler == nil {
			return nil, fmt.Errorf("unknown test destination")
		}
		request := r.Clone(r.Context())
		request.RequestURI = r.URL.RequestURI()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	nodes := make([]*Availability, size)
	for i := range nodes {
		book, err := OpenBook(bookTestDir(t), fmt.Sprintf("node-%d.test:7443", i))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { book.Close() })
		node := NewAvailability(book, nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
		nodes[i] = node
		network.handlers[node.self] = node.Handler()
	}
	return nodes, network
}

func (n *availabilityNetwork) total() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.requests
}

func learnAvailabilityPeers(t *testing.T, node *Availability, peers ...*Availability) {
	t.Helper()
	for _, peer := range peers {
		if _, err := node.book.Merge(Message{Address: peer.self}); err != nil {
			t.Fatal(err)
		}
	}
}

func relayOnce(node *Availability, now time.Time) {
	cursor := 0
	node.relay(context.Background(), node.book.Snapshot().Peers, make(map[string]*peerSchedule), &cursor, now)
}

func putAvailabilityBody(t *testing.T, store *cache.Store, key, body string) {
	t.Helper()
	digest := sha256.Sum256([]byte(body))
	fill, err := store.Begin(cache.Metadata{Key: key, Digest: fmt.Sprintf("sha256:%x", digest), Size: int64(len(body)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fill, body); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
}
