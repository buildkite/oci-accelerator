package peering

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestNewProviderHintsRejectsNonpositiveDurations(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	for name, index := range map[string]int{"timeout": 0, "refresh": 1, "full refresh": 2} {
		for _, value := range []time.Duration{0, -time.Nanosecond} {
			t.Run(name+"="+value.String(), func(t *testing.T) {
				durations := [3]time.Duration{time.Minute, time.Minute, time.Hour}
				durations[index] = value
				defer func() {
					if recover() == nil {
						t.Fatal("constructor accepted a nonpositive duration")
					}
				}()
				NewProviderHints(a.book, a.hints.client, nil, durations[0], durations[1], durations[2], a.hints.logger)
			})
		}
	}
}

func TestProviderHintsRefreshAddsPeerHints(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 3)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)

	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3} {
		assertHintProviders(t, a.hints, testCacheKey(i), b.id)
	}

	// B evicted keys 1 and 2 and cached key 4.
	b.setInventory(testInventory([]cacheKey{testCacheKey(3), testCacheKey(4)}))
	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
		t.Fatal(err)
	}
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	assertHintProviders(t, a.hints, testCacheKey(2), b.id)
	assertHintProviders(t, a.hints, testCacheKey(3), b.id)
	assertHintProviders(t, a.hints, testCacheKey(4), b.id)
}

func TestProviderHintsKeepsOtherPeersHints(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 2)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(2, 2)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	a.learn(t, c)
	for _, peer := range []*hintsTestNode{b, c} {
		if err := a.hints.refresh(context.Background(), peer.id, new(cache.KeyPosition)); err != nil {
			t.Fatal(err)
		}
	}
	b.setInventory(nil)
	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
		t.Fatal(err)
	}
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	want := []string{b.id, c.id}
	slices.Sort(want)
	assertHintProviders(t, a.hints, testCacheKey(2), want...)
	assertHintProviders(t, a.hints, testCacheKey(3), c.id)
}

func TestProviderHintsFiltersDepartedProvidersAndRetainsRejoinedClaims(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(1, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	for _, peer := range []*hintsTestNode{b, c} {
		a.learn(t, peer)
		if err := a.hints.refresh(context.Background(), peer.id, new(cache.KeyPosition)); err != nil {
			t.Fatal(err)
		}
	}
	// Another node takes B's address, removing B's ID from the book.
	if _, err := a.book.merge(Message{Address: b.address, NodeID: testNodeID(9), Peers: map[string]string{}}, b.address, nil); err != nil {
		t.Fatal(err)
	}
	assertHintProviders(t, a.hints, testCacheKey(1), c.id)
	a.learn(t, b)
	want := []string{b.id, c.id}
	slices.Sort(want)
	assertHintProviders(t, a.hints, testCacheKey(1), want...)
	if b.requests.Load() != 1 {
		t.Fatal("rejoined claim required another fetch")
	}
}

func TestProviderHintsFailedRefreshKeepsPreviousAndAppliedHints(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, maxKeysPerPage+1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err != nil {
		t.Fatal(err)
	}

	// The second page fails, but the fully validated first page remains.
	b.setInventory(testInventory(testCacheKeys(100_000, maxKeysPerPage+1)))
	b.failAfter.Store(b.requests.Load() + 1)
	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err == nil {
		t.Fatal("refresh succeeded despite a failed page")
	}
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage+1), b.id)
	assertHintProviders(t, a.hints, testCacheKey(100_000), b.id)
	assertHintProviders(t, a.hints, testCacheKey(100_000+maxKeysPerPage))
}

func TestProviderHintsDeadlineSpansPagesAndRetainsProgress(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	b := newHintsTestNode(t, "b.test:1", nil)
	a.learn(t, b)
	synctest.Test(t, func(t *testing.T) {
		a.hints.refreshTimeout = time.Second
		requests := 0
		a.hints.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			encoded, err := encodeKeysPage(keysPage{node: b.hints.self, keys: testCacheKeys((requests-1)*maxKeysPerPage, maxKeysPerPage), next: testKeyPosition(requests*maxKeysPerPage, testCacheKey(requests*maxKeysPerPage-1))})
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(encoded))}, nil
		})}
		start := time.Now()
		if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("refresh = %v, want deadline", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second || requests != 3 {
			t.Fatalf("elapsed = %v, requests = %d", elapsed, requests)
		}
		assertHintProviders(t, a.hints, testCacheKey(0), b.id)
		assertHintProviders(t, a.hints, testCacheKey(2*maxKeysPerPage-1), b.id)
		assertHintProviders(t, a.hints, testCacheKey(2*maxKeysPerPage))
	})
}

func TestProviderHintsRefreshRejectsReusedAddress(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(1, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	// The book still maps C's address to B, as after C took over B's address.
	if _, err := a.book.merge(Message{Address: c.address, NodeID: b.id, Peers: map[string]string{}}, c.address, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.hints.refresh(context.Background(), b.id, new(cache.KeyPosition)); err == nil {
		t.Fatal("refresh credited another node's keys")
	}
	assertHintProviders(t, a.hints, testCacheKey(1))
	if c.requests.Load() != 1 {
		t.Fatalf("requests to reused address = %d, want 1", c.requests.Load())
	}
}

func TestProviderHintsRefreshRejectsUnknownPeersBeforeSending(t *testing.T) {
	var requests atomic.Int32
	a := newHintsTestNode(t, "a.test:1", nil)
	a.hints.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected request")
	})}
	for name, peer := range map[string]string{"invalid ID": "short", "self": a.id, "unknown": testNodeID(9)} {
		if err := a.hints.refresh(context.Background(), peer, new(cache.KeyPosition)); !errors.Is(err, errUnknownPeer) {
			t.Errorf("%s: refresh = %v, want %v", name, err, errUnknownPeer)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestProviderHintsRefreshReportsCancellation(t *testing.T) {
	// Two pages, so the request after the cancellation fails for certain.
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, maxKeysPerPage+1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.hints.refresh(ctx, b.id, new(cache.KeyPosition)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v, want %v", err, context.Canceled)
	}
	if b.requests.Load() != 0 {
		t.Fatalf("canceled refresh sent %d requests", b.requests.Load())
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	b.setBlock(cancel)
	if err := a.hints.refresh(ctx, b.id, new(cache.KeyPosition)); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh canceled during fetch = %v, want %v", err, context.Canceled)
	}
	assertHintProviders(t, a.hints, testCacheKey(1))
}

func TestProviderHintsProvidersRejectsInvalidKeys(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	for _, key := range []string{"", "abc", testCacheKey(1).String() + "0"} {
		if got := a.hints.Providers(key); got != nil {
			t.Errorf("Providers(%q) = %v", key, got)
		}
	}
}

func TestPeerHandlerRoutesCacheKeysAndEntries(t *testing.T) {
	var routed []string
	handler := PeerHandler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { routed = append(routed, "discovery") }),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { routed = append(routed, "keys") }),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { routed = append(routed, "entries") }),
	)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, target := range []string{keysPath, keysPath + "?after=x", entryPath, entryPath + "?x=y", "/v1/peers", "/v1/cache/keys/", "/v1/cache/entry/", "/"} {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, target, nil))
		}
	}
	want := []string{"keys", "keys", "entries", "entries", "discovery", "discovery", "discovery", "discovery"}
	want = append(want, want...)
	if !reflect.DeepEqual(routed, want) {
		t.Fatalf("routed = %v, want %v", routed, want)
	}
}

// hintsTestNode is one node's book and hints, served over TLS with its
// inventory. Requests numbered above failAfter, when set, fail. A block
// function, when set, runs at the start of each request.
type hintsTestNode struct {
	id, address string
	book        *Book
	hints       *ProviderHints
	requests    atomic.Int32
	failAfter   atomic.Int32
	block       atomic.Pointer[func()]

	mu        sync.Mutex
	inventory []string
}

func newHintsTestNode(t *testing.T, advertise string, inventory []string) *hintsTestNode {
	t.Helper()
	book, err := OpenBook(context.Background(), bookTestDir(t), advertise, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	n := &hintsTestNode{id: book.Snapshot().NodeID, book: book, inventory: inventory}
	server := httptest.NewUnstartedServer(nil)
	server.StartTLS()
	t.Cleanup(server.Close)
	n.hints = NewProviderHints(book, server.Client(), n.currentInventory, time.Minute, time.Minute, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	keys := n.hints.Handler()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block := n.block.Load(); block != nil {
			(*block)()
		}
		if count := n.requests.Add(1); n.failAfter.Load() != 0 && count > n.failAfter.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		keys.ServeHTTP(w, r)
	})
	n.address = testServerAddress(server)
	return n
}

// learn records peer's address and ID and delivers discovery's notification.
func (n *hintsTestNode) learn(t *testing.T, peer *hintsTestNode) {
	t.Helper()
	result, err := n.book.merge(Message{Address: peer.address, NodeID: peer.id, Peers: map[string]string{}}, peer.address, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.hints.PeersAdded(result.Added)
}

func (n *hintsTestNode) setBlock(block func()) { n.block.Store(&block) }

func (n *hintsTestNode) setInventory(inventory []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.inventory = inventory
}

func (n *hintsTestNode) currentInventory(after cache.KeyPosition, limit int) ([]string, cache.KeyPosition, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return testKeyPages(n.inventory)(after, limit)
}

func assertHintProviders(t *testing.T, hints *ProviderHints, key cacheKey, want ...string) {
	t.Helper()
	if got := hints.Providers(key.String()); !slices.Equal(got, want) {
		t.Fatalf("Providers(%s) = %q, want %q", key, got, want)
	}
}
