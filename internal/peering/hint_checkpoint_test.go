package peering

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProviderHintsCheckpointRestoresStateAcrossRestart(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	completed, pending := testCacheKey(1), testCacheKey(2)
	inventory.set(completed.String(), pending.String())
	a.reconcile(inventory.Keys())
	remote := testHint(t, testIDA, testCacheKey(3), testIDA)
	if err := a.accept(announcement{sender: remote.provider, recipient: a.self, hints: []announcedHint{remote}}); err != nil {
		t.Fatal(err)
	}
	peer := mustNodeID(t, testIDA)
	a.mu.Lock()
	done := a.index.find(a.self, completed)
	a.index.addSeen(done, []nodeID{peer})
	done.ack(peer)
	done.attempts = 1
	a.index.finishPass(done)
	open := a.index.find(a.self, pending)
	a.index.addSeen(open, []nodeID{peer})
	open.ack(peer)
	open.attempts = 4
	a.revision++
	a.mu.Unlock()
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(a.book.dir, hintSnapshotName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %v, err %v", info.Mode(), err)
	}
	want := fifoSummary(a)

	restarted := reopenProviderHints(t, a, inventory)
	if got := fifoSummary(restarted); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored FIFO\n%v\nwant\n%v", got, want)
	}
	if restarted.dirty() {
		t.Fatal("a clean restart reannounced retained local keys")
	}
	h := restarted.index.find(restarted.self, pending)
	if !h.pending || h.queued < 0 || h.attempts != 4 || !h.hasAcked(peer) || h.inflight != 0 {
		t.Fatalf("pending pass after restart = %+v", h)
	}
	if h := restarted.index.find(restarted.self, completed); h.pending || h.queued >= 0 {
		t.Fatal("a completed pass restarted")
	}
	if got := restarted.Providers(testCacheKey(3).String()); !slices.Equal(got, []string{testIDA}) {
		t.Fatalf("remote providers after restart = %v", got)
	}
}

func TestProviderHintsRestartReconcilesInventory(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	kept, removed, added := testCacheKey(1), testCacheKey(2), testCacheKey(3)
	inventory.set(kept.String(), removed.String())
	a.reconcile(inventory.Keys())
	remote := testHint(t, testIDA, removed, testIDA)
	if err := a.accept(announcement{sender: remote.provider, recipient: a.self, hints: []announcedHint{remote}}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.index.finishPass(a.index.find(a.self, kept))
	a.mu.Unlock()
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	inventory.set(kept.String(), added.String())
	restarted := reopenProviderHints(t, a, inventory)
	if h := restarted.index.find(restarted.self, kept); h == nil || h.pending {
		t.Fatalf("retained local hint = %+v", h)
	}
	if restarted.index.find(restarted.self, removed) != nil {
		t.Fatal("local hint for an absent key survived restart")
	}
	if h := restarted.index.find(restarted.self, added); h == nil || !h.pending {
		t.Fatalf("new key after restart = %+v", h)
	}
	if got := restarted.Providers(removed.String()); !slices.Equal(got, []string{testIDA}) {
		t.Fatalf("startup reconciliation changed remote hints: %v", got)
	}
	if !restarted.dirty() {
		t.Fatal("startup changes are not marked for checkpointing")
	}
}

func TestProviderHintsDiscardsUnusableSnapshots(t *testing.T) {
	// Each case is named after the reason it logs.
	cases := map[string]func(t *testing.T, a *ProviderHints) []byte{
		"corrupt": func(*testing.T, *ProviderHints) []byte { return []byte(`{"version":1,`) },
		"unsupported version": func(*testing.T, *ProviderHints) []byte {
			return []byte(`{"version":9,"nodeId":"` + testIDC + `","hints":[]}`)
		},
		"another node ID": func(t *testing.T, a *ProviderHints) []byte {
			other := newHintIndex(mustNodeID(t, testIDB))
			other.announce(testCacheKey(1), time.Time{})
			return encodeTestSnapshot(t, mustNodeID(t, testIDB), other)
		},
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
			if _, err := a.book.Merge(Message{Address: "a.test:443", NodeID: testIDA, Peers: map[string]string{}}); err != nil {
				t.Fatal(err)
			}
			dir := a.book.dir
			if err := os.WriteFile(filepath.Join(dir, hintSnapshotName), snapshot(t, a), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			a.book.Close()
			book, err := OpenBook(dir, "c.test:443")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			restarted, err := NewProviderHints(book, inventory, nil, 30*time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if restarted.index.len() != 0 {
				t.Fatalf("restored %d hints from an unusable snapshot", restarted.index.len())
			}
			if !strings.Contains(logs.String(), "provider hint state discarded") || !strings.Contains(logs.String(), name) || strings.Contains(logs.String(), testCacheKey(1).String()) {
				t.Fatalf("logs = %q", logs.String())
			}
			// Identity and the address book are untouched.
			if got := book.Snapshot(); got.NodeID != testIDC || got.Peers["a.test:443"] != testIDA {
				t.Fatalf("book after discard = %+v", got)
			}
			if err := restarted.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if _, err := loadHintSnapshot(dir, restarted.self); err != nil {
				t.Fatalf("discarded snapshot was not replaced: %v", err)
			}
		})
	}
}

func TestProviderHintsUnsafeSnapshotFailsStartup(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	inventory.set(testCacheKey(1).String())
	a.reconcile(inventory.Keys())
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	dir := a.book.dir
	a.book.Close()
	if err := os.Chmod(filepath.Join(dir, hintSnapshotName), 0o644); err != nil {
		t.Fatal(err)
	}
	if book, err := OpenBook(dir, "c.test:443"); err == nil {
		book.Close()
		t.Fatal("startup accepted a group-readable provider hint snapshot")
	}
}

func TestProviderHintsCheckpointFailureKeepsStateDirty(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&logs, nil))
	inventory.set(testCacheKey(1).String())
	a.reconcile(inventory.Keys())
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.book.dir, hintSnapshotName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A write that fails partway, as on a full disk, leaves the old snapshot.
	healthy := a.persist
	a.persist = func(write func(io.Writer) error) error {
		_, err := replaceStateWith(a.book.dir, hintSnapshotName, func(w io.Writer) error {
			_, _ = w.Write([]byte(`{"version":1,`))
			return &os.PathError{Op: "write", Path: "/private/path", Err: syscall.ENOSPC}
		}, syncBookDir)
		return err
	}
	inventory.set(testCacheKey(1).String(), testCacheKey(2).String())
	a.reconcile(inventory.Keys())
	for range 3 {
		if err := a.Checkpoint(); !errors.Is(err, errHintCheckpoint) {
			t.Fatalf("checkpoint err = %v", err)
		}
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("failed checkpoint changed the snapshot: %v", err)
	}
	assertNoStagingFiles(t, a.book.dir)
	if !a.dirty() || a.Stats().CheckpointFailures != 3 {
		t.Fatalf("dirty %v, failures %d", a.dirty(), a.Stats().CheckpointFailures)
	}
	if n := strings.Count(logs.String(), "provider hint checkpoint failed"); n != 1 {
		t.Fatalf("logged %d failures", n)
	}
	if !strings.Contains(logs.String(), "no space left on device") || strings.Contains(logs.String(), "/private/path") {
		t.Fatalf("failure log = %q", logs.String())
	}

	a.persist = healthy
	if err := a.Checkpoint(); err != nil || a.dirty() {
		t.Fatalf("recovery checkpoint err %v, dirty %v", err, a.dirty())
	}
	if !strings.Contains(logs.String(), "provider hint checkpoint recovered") {
		t.Fatal("recovery was not logged")
	}
	hints, err := loadHintSnapshot(a.book.dir, a.self)
	if err != nil || len(hints) != 2 {
		t.Fatalf("recovered snapshot has %d hints, err %v", len(hints), err)
	}
}

func TestProviderHintsDuplicateHintsLeaveStateClean(t *testing.T) {
	a, _ := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	announced := testHint(t, testIDA, testCacheKey(1), testIDA)
	message := announcement{sender: announced.provider, recipient: a.self, hints: []announcedHint{announced}}
	if err := a.accept(message); err != nil {
		t.Fatal(err)
	}
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// A duplicate that adds nothing does not force another rewrite.
	if err := a.accept(message); err != nil {
		t.Fatal(err)
	}
	if a.dirty() {
		t.Fatal("a duplicate hint dirtied the state")
	}
}

func TestProviderHintsCheckpointDuringMutationStaysDirty(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	inventory.set(testCacheKey(1).String())
	a.reconcile(inventory.Keys())
	healthy := a.persist
	writing, release := make(chan struct{}), make(chan struct{})
	a.persist = func(write func(io.Writer) error) error {
		close(writing)
		<-release
		return healthy(write)
	}
	done := make(chan error, 1)
	go func() { done <- a.Checkpoint() }()
	<-writing
	// Mutations proceed while the checkpoint writes without the lock.
	announced := testHint(t, testIDA, testCacheKey(2), testIDA)
	if err := a.accept(announcement{sender: announced.provider, recipient: a.self, hints: []announcedHint{announced}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !a.dirty() {
		t.Fatal("an older checkpoint marked newer state clean")
	}
	a.persist = healthy
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if hints, err := loadHintSnapshot(a.book.dir, a.self); err != nil || len(hints) != 2 {
		t.Fatalf("snapshot has %d hints, err %v", len(hints), err)
	}
}

func TestProviderHintsRunCheckpointsPeriodicallyAndRetriesFailures(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	a.checkpointEvery = 20 * time.Millisecond
	var mu sync.Mutex
	failing := false
	healthy := a.persist
	a.persist = func(write func(io.Writer) error) error {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return errors.New("write failed")
		}
		return healthy(write)
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
	eventually(t, 5*time.Second, func() bool {
		hints, err := loadHintSnapshot(a.book.dir, a.self)
		return err == nil && len(hints) == 1
	})

	mu.Lock()
	failing = true
	mu.Unlock()
	inventory.set(testCacheKey(1).String(), testCacheKey(2).String())
	eventually(t, 5*time.Second, func() bool { return a.Stats().CheckpointFailures >= 2 })
	start, failures := time.Now(), a.Stats().CheckpointFailures
	time.Sleep(200 * time.Millisecond)
	// Retries wait for the interval rather than looping.
	if extra := a.Stats().CheckpointFailures - failures; extra > uint64(time.Since(start)/a.checkpointEvery)+1 {
		t.Fatalf("%d retries in %v", extra, time.Since(start))
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	eventually(t, 5*time.Second, func() bool {
		hints, err := loadHintSnapshot(a.book.dir, a.self)
		return err == nil && len(hints) == 2
	})
}

func TestProviderHintsInterruptedWriteLeavesUsableSnapshot(t *testing.T) {
	a, inventory := newTestProviderHints(t, testIDC, "c.test:443", time.Now, 1)
	inventory.set(testCacheKey(1).String())
	a.reconcile(inventory.Keys())
	if err := a.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves a partial staging file beside the snapshot.
	if err := os.WriteFile(filepath.Join(a.book.dir, ".peers-interrupted"), []byte(`{"version":1,"hin`), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := reopenProviderHints(t, a, inventory)
	assertNoStagingFiles(t, restarted.book.dir)
	if restarted.index.find(restarted.self, testCacheKey(1)) == nil || restarted.dirty() {
		t.Fatal("previous snapshot was not restored after an interrupted write")
	}
}

// reopenProviderHints simulates a restart: it releases a's book and opens a
// new book and provider hints on the same state directory.
func reopenProviderHints(t *testing.T, a *ProviderHints, inventory Inventory) *ProviderHints {
	t.Helper()
	dir, address := a.book.dir, a.book.self
	if err := a.book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err := OpenBook(dir, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	restarted, err := NewProviderHints(book, inventory, nil, 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return restarted
}

// fifoSummary returns each retained hint's persisted fields in FIFO order.
func fifoSummary(a *ProviderHints) []hintState {
	a.mu.Lock()
	defer a.mu.Unlock()
	var summary []hintState
	for e := a.index.fifo.Front(); e != nil; e = e.Next() {
		summary = append(summary, encodeHintState(e.Value.(*hint)))
	}
	return summary
}

func assertNoStagingFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".peers-") {
			t.Fatalf("staging file %s remains", entry.Name())
		}
	}
}
