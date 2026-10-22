package peering

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestProviderHintsRunRefreshesPeersKnownAtStartup(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(2, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	a.learn(t, c)
	// A restart builds new hints from a book that already knows B and C.
	a.hints = NewProviderHints(a.book, a.hints.client, a.currentInventory, time.Minute, time.Minute, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	stop := runHints(t, a.hints)
	eventually(t, 3*time.Second, func() bool {
		return len(a.hints.Providers(testCacheKey(1).String())) == 1 && len(a.hints.Providers(testCacheKey(2).String())) == 1
	})
	stop()
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	assertHintProviders(t, a.hints, testCacheKey(2), c.id)
	if b.requests.Load() != 1 || c.requests.Load() != 1 {
		t.Fatalf("requests = %d to B and %d to C, want 1 each", b.requests.Load(), c.requests.Load())
	}
}

func TestProviderHintsPeersAddedDoesNotWaitForIndex(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	a.hints.mu.Lock() // as during a large index replacement
	defer a.hints.mu.Unlock()
	queued := make(chan struct{})
	go func() {
		a.hints.PeersAdded([]string{testNodeID(1)})
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(3 * time.Second):
		t.Fatal("PeersAdded waited for the hint index")
	}
}

func TestProviderHintsRunRefreshesDiscoveredPeers(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	stop := runHints(t, a.hints)
	a.learn(t, b)
	eventually(t, 3*time.Second, func() bool { return len(a.hints.Providers(testCacheKey(1).String())) == 1 })
	stop()
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	if b.requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", b.requests.Load())
	}
}

func TestProviderHintsRunCatchesUpIncrementallyAndRescans(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(0, maxKeysPerPage+2)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	synctest.Test(t, func(t *testing.T) {
		a.hints.wake = make(chan struct{}, 1)
		a.hints.refreshInterval = 10 * time.Second
		a.hints.fullRefreshInterval = time.Minute
		var requests hintRequestLog
		handler := b.hints.Handler()
		a.hints.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.RequestURI = r.URL.RequestURI()
			after, _ := parseKeysRequest(r)
			requests.record(after)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			return response.Result(), nil
		})}
		stop := runHints(t, a.hints)
		synctest.Wait()
		starts := requests.snapshot()
		if len(starts) != 2 {
			t.Fatalf("startup requests = %d, want 2", len(starts))
		}
		assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage+1), b.id)
		b.setInventory(testInventory(testCacheKeys(0, maxKeysPerPage+3)))
		// Unrelated discovery notifications must not bypass the poll interval.
		for range 20 {
			a.hints.PeersAdded([]string{testNodeID(999)})
		}
		time.Sleep(7 * time.Second)
		synctest.Wait()
		starts = requests.snapshot()
		if len(starts) != 2 {
			t.Fatalf("early requests = %d, want 2", len(starts))
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		starts = requests.snapshot()
		if len(starts) != 3 || starts[2].Last != maxKeysPerPage {
			t.Fatalf("incremental requests = %v", starts)
		}
		assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage+2), b.id)
		// A lost cold-key hint is not repaired by tail polling, but a full
		// scan must restore it. Simulate eviction from the bounded hint index.
		a.hints.mu.Lock()
		a.hints.index = newHintIndex(maxHintRecords)
		a.hints.mu.Unlock()
		time.Sleep(13 * time.Second)
		synctest.Wait()
		assertHintProviders(t, a.hints, testCacheKey(0))
		assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage+2), b.id)
		time.Sleep(60 * time.Second)
		synctest.Wait()
		assertHintProviders(t, a.hints, testCacheKey(0), b.id)
		stop()
	})
}

func TestProviderHintsRunResumesFailuresAndRecoversFromPeerRestart(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", nil)
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	synctest.Test(t, func(t *testing.T) {
		a.hints.wake = make(chan struct{}, 1)
		a.hints.refreshInterval = 10 * time.Second
		a.hints.fullRefreshInterval = time.Hour
		var logs bytes.Buffer
		a.hints.logger = slog.New(slog.NewTextHandler(&logs, nil))
		var inventoryMu sync.Mutex
		generation := byte(1)
		inventory := testInventory(testCacheKeys(0, 2*maxKeysPerPage+3))
		handler := newKeysHandler(b.hints.self, func(after cache.KeyPosition, limit int) ([]string, cache.KeyPosition, error) {
			inventoryMu.Lock()
			defer inventoryMu.Unlock()
			if after != (cache.KeyPosition{}) {
				if after.Incarnation != ([16]byte{generation}) {
					return nil, cache.KeyPosition{}, cache.ErrKeyCursor
				}
				after.Incarnation = [16]byte{1}
			}
			keys, next, err := testKeyPages(inventory)(after, limit)
			if next != (cache.KeyPosition{}) {
				next.Incarnation = [16]byte{generation}
			}
			return keys, next, err
		})
		var requests hintRequestLog
		a.hints.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.RequestURI = r.URL.RequestURI()
			after, _ := parseKeysRequest(r)
			count := requests.record(after)
			response := httptest.NewRecorder()
			if count == 2 {
				http.Error(response, "unavailable", http.StatusServiceUnavailable)
			} else {
				handler.ServeHTTP(response, r)
			}
			return response.Result(), nil
		})}
		stop := runHints(t, a.hints)
		synctest.Wait()
		assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage-1), b.id)
		assertHintProviders(t, a.hints, testCacheKey(maxKeysPerPage))
		// Stop at the next completed attempt, regardless of its jittered
		// deadline, without advancing far enough to run a second poll.
		nextPoll := func() {
			t.Helper()
			before := len(requests.snapshot())
			for range 13 {
				time.Sleep(time.Second)
				synctest.Wait()
				if len(requests.snapshot()) > before {
					return
				}
			}
			t.Fatal("no poll within the jittered interval")
		}
		nextPoll()
		starts := requests.snapshot()
		if len(starts) != 4 || starts[2].Last != maxKeysPerPage {
			t.Fatalf("resumed requests = %v", starts)
		}
		assertHintProviders(t, a.hints, testCacheKey(2*maxKeysPerPage+2), b.id)
		inventoryMu.Lock()
		generation = 2
		inventory = testInventory(testCacheKeys(100_000, 2*maxKeysPerPage+3))
		inventoryMu.Unlock()
		before := len(requests.snapshot())
		nextPoll()
		starts = requests.snapshot()
		if len(starts) != before+1 || starts[before].Last == 0 {
			t.Fatalf("restart rejection requests = %v", starts[before:])
		}
		assertHintProviders(t, a.hints, testCacheKey(100_000))
		nextPoll()
		starts = requests.snapshot()
		if starts[before+1] != (cache.KeyPosition{}) {
			t.Fatal("rejected cursor was not reset")
		}
		assertHintProviders(t, a.hints, testCacheKey(100_000), b.id)
		assertHintProviders(t, a.hints, testCacheKey(100_000+2*maxKeysPerPage+2), b.id)
		assertHintProviders(t, a.hints, testCacheKey(0), b.id)
		stop()
		if strings.Count(logs.String(), "peer cache keys refresh failed") != 2 || !strings.Contains(logs.String(), b.id) {
			t.Fatalf("failure log = %q", logs.String())
		}
	})
}

func TestProviderHintsRunBoundsConcurrencyAndJoinsOnCancellation(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	for i := 1; i <= 6; i++ {
		result, err := a.book.merge(Message{Address: fmt.Sprintf("peer%d.test:1", i), NodeID: testNodeID(i), Peers: map[string]string{}}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		a.hints.PeersAdded(result.Added)
	}
	synctest.Test(t, func(t *testing.T) {
		a.hints.wake = make(chan struct{}, 1)
		a.hints.refreshInterval = 10 * time.Second
		a.hints.refreshTimeout = time.Minute
		var mu sync.Mutex
		active := make(map[string]bool)
		seen := make(map[string]bool)
		peak := 0
		release := make(chan struct{})
		peers := a.book.Snapshot().Peers
		a.hints.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.RequestURI = r.URL.RequestURI()
			mu.Lock()
			if active[r.URL.Host] {
				t.Error("overlapping requests for the same peer")
			}
			active[r.URL.Host] = true
			seen[r.URL.Host] = true
			peak = max(peak, len(active))
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(active, r.URL.Host)
				mu.Unlock()
			}()
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-release:
				response := httptest.NewRecorder()
				id, _ := parseNodeID(peers[r.URL.Host])
				newKeysHandler(id, testKeyPages(testInventory([]cacheKey{testCacheKey(1)}))).ServeHTTP(response, r)
				return response.Result(), nil
			}
		})}
		stop := runHints(t, a.hints)
		synctest.Wait()
		if len(active) != 4 || len(seen) != 4 {
			t.Fatalf("initial active = %d, seen = %d, want 4 each", len(active), len(seen))
		}
		for range 20 {
			a.hints.PeersAdded([]string{testNodeID(999)})
		}
		synctest.Wait()
		release <- struct{}{}
		synctest.Wait()
		if len(active) != 4 || len(seen) != 5 {
			t.Fatalf("after release active = %d, seen = %d, want 4 and 5", len(active), len(seen))
		}
		time.Sleep(20 * time.Second)
		release <- struct{}{}
		synctest.Wait()
		if len(seen) != 6 || peak != 4 {
			t.Fatalf("seen = %d, peak = %d, want 6 and 4", len(seen), peak)
		}
		stop()
		if len(active) != 0 {
			t.Fatal("Run returned before its requests stopped")
		}
	})
}

func TestProviderHintsRunRescansRejoiningIdentity(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(fmt.Sprintf("in-flight=%t", inFlight), func(t *testing.T) {
			b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(0, maxKeysPerPage+2)))
			a := newHintsTestNode(t, "a.test:1", nil)
			a.learn(t, b)
			synctest.Test(t, func(t *testing.T) {
				a.hints.wake = make(chan struct{}, 1)
				a.hints.refreshInterval = 10 * time.Second
				var requests hintRequestLog
				var blockMu sync.Mutex
				blockNext := false
				release := make(chan struct{})
				handler := b.hints.Handler()
				a.hints.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					r.RequestURI = r.URL.RequestURI()
					after, _ := parseKeysRequest(r)
					requests.record(after)
					blockMu.Lock()
					block := blockNext
					blockNext = false
					blockMu.Unlock()
					if block {
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, r)
					return response.Result(), nil
				})}
				stop := runHints(t, a.hints)
				synctest.Wait()
				assertHintProviders(t, a.hints, testCacheKey(0), b.id)
				if inFlight {
					blockMu.Lock()
					blockNext = true
					blockMu.Unlock()
					time.Sleep(13 * time.Second)
					synctest.Wait()
				}
				a.hints.mu.Lock()
				a.hints.index = newHintIndex(maxHintRecords)
				a.hints.mu.Unlock()
				// B disappears and returns before the scheduler observes a
				// snapshot. Deliver the resulting discovery additions together.
				var added []string
				for _, id := range []string{testNodeID(999), b.id} {
					result, err := a.book.merge(Message{Address: b.address, NodeID: id, Peers: map[string]string{}}, "", nil)
					if err != nil {
						t.Fatal(err)
					}
					added = append(added, result.Added...)
				}
				before := len(requests.snapshot())
				a.hints.PeersAdded(added)
				synctest.Wait()
				if inFlight {
					if len(requests.snapshot()) != before {
						t.Fatal("rejoin overlapped the earlier fetch")
					}
					assertHintProviders(t, a.hints, testCacheKey(0))
					close(release)
					synctest.Wait()
				}
				assertHintProviders(t, a.hints, testCacheKey(0), b.id)
				positions := requests.snapshot()
				if len(positions) != before+2 || positions[before] != (cache.KeyPosition{}) {
					t.Fatalf("rejoin did not start a fresh full scan: %v", positions[before:])
				}
				stop()
			})
		})
	}
}

func TestProviderHintsRunStopsWhenIdle(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.hints.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// runHints runs hints until the returned stop function or test cleanup
// cancels and joins it.
func runHints(t *testing.T, hints *ProviderHints) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); hints.Run(ctx) }()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

// hintRequestLog captures observable request positions across polling turns.
type hintRequestLog struct {
	mu        sync.Mutex
	positions []cache.KeyPosition
}

func (l *hintRequestLog) record(after cache.KeyPosition) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.positions = append(l.positions, after)
	return len(l.positions)
}

func (l *hintRequestLog) snapshot() []cache.KeyPosition {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.positions)
}
