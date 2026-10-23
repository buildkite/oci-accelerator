package peering

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
)

func TestBookMergePersistsOnlyCanonicalSender(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(context.Background(), dir, "SELF.TEST.:443", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	self := b.Snapshot().NodeID
	result, err := b.merge(Message{Address: "B.TEST:2", NodeID: testNodeID(2), Peers: map[string]string{"A.TEST.:01": testNodeID(1), "SELF.TEST:443": self}}, "b.test:2", nil)
	if err != nil || result.Rejected != 0 || !reflect.DeepEqual(result.Verify, []string{"a.test:1"}) {
		t.Fatalf("Merge = %+v, %v", result, err)
	}
	want := Message{Address: "self.test:443", NodeID: self, Peers: map[string]string{"b.test:2": testNodeID(2)}}
	if got := b.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot = %#v, want %#v", got, want)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBook(context.Background(), dir, "self.test:443", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := b.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened Snapshot = %#v, want %#v", got, want)
	}
}

func TestBookMergeReportsAddedIDs(t *testing.T) {
	b, err := OpenBook(context.Background(), bookTestDir(t), "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.AddSeeds([]string{"seed.test:1"}); err != nil {
		t.Fatal(err)
	}
	merge := func(message Message) []string {
		t.Helper()
		if message.Peers == nil {
			message.Peers = map[string]string{}
		}
		result, err := b.merge(message, message.Address, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.Added
	}
	tests := []struct {
		name    string
		message Message
		want    []string
	}{
		{"sender and third parties", Message{Address: "b.test:1", NodeID: testNodeID(2), Peers: map[string]string{"d.test:1": testNodeID(4), "c.test:1": testNodeID(3)}}, []string{testNodeID(2)}},
		{"repeated exchange", Message{Address: "b.test:1", NodeID: testNodeID(2), Peers: map[string]string{"c.test:1": testNodeID(3)}}, nil},
		{"known ID moves", Message{Address: "b2.test:1", NodeID: testNodeID(2)}, nil},
		{"seed resolves", Message{Address: "seed.test:1", NodeID: testNodeID(5)}, []string{testNodeID(5)}},
		{"address reused by a new ID", Message{Address: "c.test:1", NodeID: testNodeID(6)}, []string{testNodeID(6)}},
	}
	for _, test := range tests {
		if got := merge(test.message); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: added = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestBookReopenWithNewSelfAddress(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(context.Background(), dir, "old-self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	self := b.Snapshot().NodeID
	if _, err := b.merge(Message{Address: "new-self.test:1", NodeID: testNodeID(1), Peers: map[string]string{"other.test:2": testNodeID(2)}}, "new-self.test:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.merge(Message{Address: "other.test:2", NodeID: testNodeID(2)}, "other.test:2", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBook(context.Background(), dir, "new-self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	want := Message{Address: "new-self.test:1", NodeID: self, Peers: map[string]string{"other.test:2": testNodeID(2)}}
	if got := b.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened address book = %#v, want %#v", got, want)
	}
}

func TestBookConcurrentMergesUnion(t *testing.T) {
	b, err := OpenBook(context.Background(), bookTestDir(t), "self.test:1", nil, nil)
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
			if _, err := b.merge(Message{Address: address, NodeID: testNodeID(i)}, address, nil); err != nil {
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
	b, err := OpenBook(context.Background(), bookTestDir(t), "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	peers := make(map[string]string, MaxPeers-1)
	for i := range MaxPeers - 1 {
		peers[fmt.Sprintf("p%04d.test:1", i)] = testNodeID(i)
	}
	for address, id := range peers {
		if _, err := b.merge(Message{Address: address, NodeID: id}, address, nil); err != nil {
			t.Fatal(err)
		}
	}
	first := testNodeID(MaxPeers)
	if result, err := b.merge(Message{Address: "first.test:1", NodeID: first, Peers: peers}, "first.test:1", nil); err != nil || result.Rejected != 0 {
		t.Fatalf("filling Merge = %+v, %v", result, err)
	}
	result, err := b.merge(Message{Address: "new-a.test:1", NodeID: testNodeID(MaxPeers + 1), Peers: map[string]string{"new-b.test:1": testNodeID(MaxPeers + 2), "p0000.test:1": testNodeID(0)}}, "new-a.test:1", nil)
	if err != nil || result.Rejected != 1 || !reflect.DeepEqual(result.Verify, []string{"new-b.test:1"}) {
		t.Fatalf("Merge = %+v, %v", result, err)
	}
	rejected, err := b.AddSeeds([]string{"new-c.test:1", "NEW-C.TEST.:1", "p0001.test:1"})
	if err != nil || rejected != 0 {
		t.Fatalf("AddSeeds = %d, %v", rejected, err)
	}
	got := b.Snapshot().Peers
	if len(got) != MaxPeers || got["first.test:1"] != first || len(b.Addresses()) != MaxPeers+1 {
		t.Fatalf("existing peers changed, count %d first %q", len(got), got["first.test:1"])
	}
}

func TestBookInvalidMessageDoesNotMutate(t *testing.T) {
	b, err := OpenBook(context.Background(), bookTestDir(t), "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.merge(Message{Address: "good.test:1", NodeID: testNodeID(1), Peers: map[string]string{"bad": testNodeID(2)}}, "", nil); err == nil {
		t.Fatal("Merge succeeded")
	}
	if len(b.Addresses()) != 0 {
		t.Fatal("invalid merge mutated book")
	}
}

func TestBookFailedPersistencePreservesMemoryAndFile(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.merge(Message{Address: "old.test:1", NodeID: testNodeID(1)}, "old.test:1", nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, bookStateName))
	if err != nil {
		t.Fatal(err)
	}
	b.write = func(string, map[string]bookRecord) (bool, error) { return false, syscall.ENOSPC }
	if result, err := b.merge(Message{Address: "new.test:1", NodeID: testNodeID(2)}, "new.test:1", nil); err == nil || result.Added != nil {
		t.Fatalf("Merge = %+v, %v, want an error and no added IDs", result, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, bookStateName))
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(b.Snapshot().Peers, map[string]string{"old.test:1": testNodeID(1)}) {
		t.Fatal("failed persistence changed state")
	}
}

func TestBookDirectorySyncFailureRetainsPublishedPeersAndRetries(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.merge(Message{Address: "old.test:1", NodeID: testNodeID(1)}, "old.test:1", nil); err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	b.write = func(dir string, peers map[string]bookRecord) (bool, error) {
		return replaceBookState(dir, peers, func(dir string) error {
			syncCalls++
			if syncCalls == 1 {
				return syscall.EIO
			}
			return syncBookDir(dir)
		})
	}
	message := Message{Address: "new.test:1", NodeID: testNodeID(2)}
	if result, err := b.merge(message, message.Address, nil); err == nil {
		t.Fatal("acknowledged an exchange whose directory sync failed")
	} else if !reflect.DeepEqual(result.Added, []string{testNodeID(2)}) {
		t.Fatalf("published identity not reported: %v", result.Added)
	}
	want := map[string]string{"new.test:1": testNodeID(2), "old.test:1": testNodeID(1)}
	if got := b.Snapshot().Peers; !reflect.DeepEqual(got, want) {
		t.Fatalf("memory lost published peers: %v", got)
	}
	if got, err := loadBookState(dir); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("disk disagrees with published state: %v, %v", got, err)
	}
	if _, err := b.merge(message, message.Address, nil); err != nil || syncCalls != 2 {
		t.Fatalf("identical exchange did not retry persistence: calls=%d, error=%v", syncCalls, err)
	}
}

func TestBookClosePreventsMergeAndReleasesLock(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil); err == nil {
		t.Fatal("second OpenBook succeeded")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.merge(Message{Address: "peer.test:1", NodeID: testNodeID(1)}, "", nil); err == nil {
		t.Fatal("Merge after Close succeeded")
	}
	reopened, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
