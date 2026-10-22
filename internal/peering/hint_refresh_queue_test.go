package peering

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestProviderHintsRunRefreshesPeersKnownAtStartup(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(2, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	a.learn(t, c)
	// A restart builds new hints from a book that already knows B and C.
	a.hints = NewProviderHints(a.book, a.hints.client, a.currentInventory, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

func TestProviderHintsQueueRefreshDoesNotWaitForIndex(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	a.hints.mu.Lock() // as during a large index replacement
	defer a.hints.mu.Unlock()
	queued := make(chan struct{})
	go func() {
		a.hints.QueueRefresh([]string{testNodeID(1)})
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(3 * time.Second):
		t.Fatal("QueueRefresh waited for the hint index")
	}
}

func TestProviderHintsRunRefreshesQueuedPeers(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	stop := runHints(t, a.hints)
	a.learn(t, b)
	a.hints.QueueRefresh([]string{b.id})
	eventually(t, 3*time.Second, func() bool { return len(a.hints.Providers(testCacheKey(1).String())) == 1 })
	stop()
	assertHintProviders(t, a.hints, testCacheKey(1), b.id)
	if b.requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", b.requests.Load())
	}
}

func TestProviderHintsRunLogsFailuresAndContinues(t *testing.T) {
	b := newHintsTestNode(t, "b.test:1", testInventory(testCacheKeys(1, 1)))
	c := newHintsTestNode(t, "c.test:1", testInventory(testCacheKeys(2, 1)))
	a := newHintsTestNode(t, "a.test:1", nil)
	a.learn(t, b)
	a.learn(t, c)
	var logs bytes.Buffer
	a.hints.logger = slog.New(slog.NewTextHandler(&logs, nil))
	b.failAfter.Store(-1)

	// The unknown peer is skipped without a warning, and B's failure does
	// not stop C's refresh.
	a.hints.QueueRefresh([]string{testNodeID(9), b.id, c.id})
	stop := runHints(t, a.hints)
	eventually(t, 3*time.Second, func() bool { return len(a.hints.Providers(testCacheKey(2).String())) == 1 })
	stop()
	assertHintProviders(t, a.hints, testCacheKey(1))
	if b.requests.Load() != 1 {
		t.Fatalf("failed peer requests = %d, want 1 attempt", b.requests.Load())
	}
	if got := logs.String(); strings.Count(got, "peer cache keys refresh failed") != 1 || !strings.Contains(got, b.id) || strings.Contains(got, testNodeID(9)) {
		t.Fatalf("logs = %q", got)
	}
}

func TestProviderHintsQueueRefreshDeduplicatesAndBounds(t *testing.T) {
	a := newHintsTestNode(t, "a.test:1", nil)
	var logs bytes.Buffer
	a.hints.logger = slog.New(slog.NewTextHandler(&logs, nil))
	a.hints.QueueRefresh([]string{testNodeID(1), testNodeID(1)})
	a.hints.QueueRefresh([]string{testNodeID(1)})
	if len(a.hints.queue) != 1 || logs.Len() != 0 {
		t.Fatalf("queue = %v, logs = %q", a.hints.queue, logs.String())
	}

	ids := make([]string, MaxPeers+1)
	for i := range ids {
		ids[i] = testNodeID(i + 2)
	}
	a.hints.QueueRefresh(ids)
	if len(a.hints.queue) != MaxPeers || !strings.Contains(logs.String(), "peer cache keys refresh queue full") {
		t.Fatalf("queue length = %d, logs = %q", len(a.hints.queue), logs.String())
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
