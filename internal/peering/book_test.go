package peering

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
)

func TestBookMergePersistsCanonicalUnion(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "SELF.TEST.:443")
	if err != nil {
		t.Fatal(err)
	}
	if rejected, err := b.Merge(Message{Address: "B.TEST:2", Peers: []string{"a.test:1", "SELF.TEST:443", "A.TEST.:01"}}); err != nil || rejected != 0 {
		t.Fatalf("Merge = %d, %v", rejected, err)
	}
	want := Message{Address: "self.test:443", Peers: []string{"a.test:1", "b.test:2"}}
	if got := b.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot = %#v, want %#v", got, want)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBook(dir, "self.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := b.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened Snapshot = %#v, want %#v", got, want)
	}
}

func TestBookReopenWithNewSelfAddress(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "old-self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Merge(Message{Address: "new-self.test:1", Peers: []string{"other.test:2"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBook(dir, "new-self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := b.Snapshot(); got.Address != "new-self.test:1" || !reflect.DeepEqual(got.Peers, []string{"other.test:2"}) {
		t.Fatalf("reopened address book = %+v", got)
	}
}

func TestBookConcurrentMergesUnion(t *testing.T) {
	b, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := fmt.Sprintf("peer-%02d.test:1", i)
			if _, err := b.Merge(Message{Address: address}); err != nil {
				t.Errorf("Merge: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := len(b.Snapshot().Peers); got != 32 {
		t.Fatalf("peer count = %d", got)
	}
}

func TestBookCapPreservesExistingAndCountsUniqueRejected(t *testing.T) {
	b, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	peers := make([]string, MaxPeers-1)
	for i := range peers {
		peers[i] = fmt.Sprintf("p%04d.test:1", i)
	}
	if _, err := b.Merge(Message{Address: "first.test:1", Peers: peers}); err != nil {
		t.Fatal(err)
	}
	rejected, err := b.Merge(Message{Address: "new-a.test:1", Peers: []string{"new-b.test:1", "NEW-B.TEST:1", "p0000.test:1"}})
	if err != nil || rejected != 2 {
		t.Fatalf("Merge = %d, %v", rejected, err)
	}
	got := b.Snapshot().Peers
	if len(got) != MaxPeers || got[0] != "first.test:1" {
		t.Fatalf("existing peers changed, count %d first %q", len(got), got[0])
	}
}

func TestBookInvalidMessageDoesNotMutate(t *testing.T) {
	b, err := OpenBook(bookTestDir(t), "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Merge(Message{Address: "good.test:1", Peers: []string{"bad"}}); err == nil {
		t.Fatal("Merge succeeded")
	}
	if len(b.Snapshot().Peers) != 0 {
		t.Fatal("invalid merge mutated book")
	}
}

func TestBookFailedPersistencePreservesMemoryAndFile(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Merge(Message{Address: "old.test:1"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, bookStateName))
	if err != nil {
		t.Fatal(err)
	}
	b.write = func(string, []string) (bool, error) { return false, syscall.ENOSPC }
	if _, err := b.Merge(Message{Address: "new.test:1"}); err == nil {
		t.Fatal("Merge succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(dir, bookStateName))
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(b.Snapshot().Peers, []string{"old.test:1"}) {
		t.Fatal("failed persistence changed state")
	}
}

func TestBookDirectorySyncFailureRetainsPublishedPeersAndRetries(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Merge(Message{Address: "old.test:1"}); err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	b.write = func(dir string, peers []string) (bool, error) {
		return replaceBookState(dir, peers, func(dir string) error {
			syncCalls++
			if syncCalls == 1 {
				return syscall.EIO
			}
			return syncBookDir(dir)
		})
	}
	message := Message{Address: "new.test:1"}
	if _, err := b.Merge(message); err == nil {
		t.Fatal("acknowledged an exchange whose directory sync failed")
	}
	want := []string{"new.test:1", "old.test:1"}
	if got := b.Snapshot().Peers; !reflect.DeepEqual(got, want) {
		t.Fatalf("memory lost published peers: %v", got)
	}
	if got, err := loadBookState(dir); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("disk disagrees with published state: %v, %v", got, err)
	}
	if _, err := b.Merge(message); err != nil || syncCalls != 2 {
		t.Fatalf("identical exchange did not retry persistence: calls=%d, error=%v", syncCalls, err)
	}
}

func TestBookClosePreventsMergeAndReleasesLock(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBook(dir, "self.test:1"); err == nil {
		t.Fatal("second OpenBook succeeded")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Merge(Message{Address: "peer.test:1"}); err == nil {
		t.Fatal("Merge after Close succeeded")
	}
	reopened, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func bookTestDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "book")
}
