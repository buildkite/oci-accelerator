package peering

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewAvailabilityOriginatesCurrentInventory(t *testing.T) {
	dir := bookTestDir(t)
	book, err := OpenBook(dir, "self.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	inventory := newTestInventory(testCacheKey(2).String(), testCacheKey(1).String(), "not a key")
	a, err := NewAvailability(book, inventory, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []cacheKey{testCacheKey(1), testCacheKey(2)} {
		h := a.index.find(a.self, key)
		if h == nil || !h.pending || h.queued < 0 {
			t.Fatalf("hint for %s = %+v", key, h)
		}
	}
	if a.index.len() != 2 {
		t.Fatalf("hints = %d", a.index.len())
	}
}

func TestAvailabilityRunForwardsInventoryChanges(t *testing.T) {
	a, inventory := newTestAvailability(t, testIDA, "a.test:443", time.Now, 1)
	if _, err := a.book.Merge(Message{Address: "b.test:443", NodeID: testIDB, Peers: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	received := make(chan availabilityMessage, 1)
	a.send = func(_ context.Context, address string, message availabilityMessage) error {
		if address != "b.test:443" {
			t.Errorf("sent to %s", address)
		}
		received <- message
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	inventory.set(testCacheKey(1).String())
	select {
	case message := <-received:
		if message.recipient != mustNodeID(t, testIDB) || len(message.claims) != 1 || message.claims[0].owner != a.self {
			t.Fatalf("message = %+v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inventory change was not forwarded")
	}
	eventually(t, 5*time.Second, func() bool { return a.Stats().Acknowledged == 1 })
	if stats := a.Stats(); stats.Hints != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAvailabilityRunBoundsConcurrencyAndJoinsRequests(t *testing.T) {
	a, inventory := newTestAvailability(t, testIDA, "a.test:443", time.Now, 1)
	for i := range 8 {
		if _, err := a.book.Merge(Message{Address: testNodeID(i) + ".test:443", NodeID: testNodeID(i), Peers: map[string]string{}}); err != nil {
			t.Fatal(err)
		}
	}
	var active, peak, finished atomic.Int32
	var once sync.Once
	saturated := make(chan struct{})
	a.send = func(ctx context.Context, _ string, _ availabilityMessage) error {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		if n == availabilityConcurrency {
			once.Do(func() { close(saturated) })
		}
		<-ctx.Done() // a slow peer holds its slot until cancellation
		active.Add(-1)
		finished.Add(1)
		return ctx.Err()
	}
	keys := make([]string, 8)
	for i := range keys {
		keys[i] = testCacheKey(i).String()
	}
	inventory.set(keys...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	select {
	case <-saturated:
	case <-time.After(5 * time.Second):
		t.Fatal("requests never filled the concurrency limit")
	}
	time.Sleep(50 * time.Millisecond) // give an extra request a chance to start
	cancel()
	<-done
	if peak.Load() != availabilityConcurrency {
		t.Fatalf("peak concurrent requests = %d", peak.Load())
	}
	if active.Load() != 0 || finished.Load() != availabilityConcurrency {
		t.Fatalf("Run returned with %d active, %d finished", active.Load(), finished.Load())
	}
}

func TestAvailabilityRunAppliesInFlightResultsOnShutdown(t *testing.T) {
	a, inventory := newTestAvailability(t, testIDA, "a.test:443", time.Now, 1)
	if _, err := a.book.Merge(Message{Address: "b.test:443", NodeID: testIDB, Peers: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	a.send = func(ctx context.Context, _ string, _ availabilityMessage) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	inventory.set(testCacheKey(1).String())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	<-done
	a.mu.Lock()
	defer a.mu.Unlock()
	h := a.index.find(a.self, testCacheKey(1))
	if h.inflight != 0 || len(a.busy) != 0 || a.failed != 0 || len(a.backoff) != 0 {
		t.Fatalf("after shutdown: inflight %d busy %d failed %d backoff %d", h.inflight, len(a.busy), a.failed, len(a.backoff))
	}
	if !h.pending || h.queued < 0 || h.attempts != hintAttempts-1 {
		t.Fatalf("pass after shutdown: pending %v queued %d attempts %d", h.pending, h.queued, h.attempts)
	}
}

func TestAvailabilityOwnersRejectsInvalidKeys(t *testing.T) {
	a, _ := newTestAvailability(t, testIDA, "a.test:443", time.Now, 1)
	if got := a.Owners("NOT-A-KEY"); got != nil {
		t.Fatalf("owners = %v", got)
	}
}
