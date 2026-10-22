package peering

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestBookMergeSenderIdentityReplacesStaleBinding(t *testing.T) {
	for name, message := range map[string]Message{
		"identity moves address":       {Address: "new.test:1", NodeID: testNodeID(1)},
		"address has another identity": {Address: "old.test:1", NodeID: testNodeID(2)},
	} {
		t.Run(name, func(t *testing.T) {
			dir := bookTestDir(t)
			b := openReconcileBook(t, dir, map[string]string{"old.test:1": testNodeID(1)})
			if result, err := b.merge(message, message.Address, nil); err != nil || result.Rejected != 0 || len(result.Verify) != 0 {
				t.Fatalf("Merge = %+v, %v", result, err)
			}
			want := map[string]string{message.Address: message.NodeID}
			assertBookPeers(t, b, want)
			if got, err := loadBookState(dir); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("persisted = %v, %v", got, err)
			}
		})
	}
}

func TestBookMergeThirdPartyConflictsRequireVerification(t *testing.T) {
	b := openReconcileBook(t, bookTestDir(t), map[string]string{"a.test:1": testNodeID(1), "b.test:1": testNodeID(2)})
	result, err := b.merge(Message{Address: "sender.test:1", NodeID: testNodeID(9), Peers: map[string]string{
		"a.test:1": testNodeID(3), // address conflict
		"c.test:1": testNodeID(2), // identity conflict
	}}, "sender.test:1", nil)
	if err != nil || result.Rejected != 0 || !reflect.DeepEqual(result.Verify, []string{"a.test:1", "c.test:1"}) {
		t.Fatalf("Merge = %+v, %v", result, err)
	}
	assertBookPeers(t, b, map[string]string{"a.test:1": testNodeID(1), "b.test:1": testNodeID(2), "sender.test:1": testNodeID(9)})
}

func TestBookGossipCannotAssignOneIDToTwoNewAddresses(t *testing.T) {
	b := openReconcileBook(t, bookTestDir(t), nil)
	result, err := b.merge(Message{Address: "sender.test:1", NodeID: testNodeID(9), Peers: map[string]string{
		"a.test:1": testNodeID(1), "b.test:1": testNodeID(1),
	}}, "sender.test:1", nil)
	if err != nil || !reflect.DeepEqual(result.Verify, []string{"a.test:1", "b.test:1"}) {
		t.Fatalf("Merge = %+v, %v", result, err)
	}
	assertBookPeers(t, b, map[string]string{"sender.test:1": testNodeID(9)})
}

func TestBookProbeRetiresAliasAndQueuesAdvertisedListener(t *testing.T) {
	b := openReconcileBook(t, bookTestDir(t), map[string]string{"alias.test:1": testNodeID(1)})
	_, view := b.observe()
	// The probe was queued by a claim that alias has identity 2, but a third
	// identity answers and advertises a different listener address.
	response := Message{Address: "real.test:1", NodeID: testNodeID(3), Peers: map[string]string{"alias.test:1": testNodeID(2)}}
	if result, err := b.merge(response, "alias.test:1", &view); err != nil || result.Rejected != 0 || !reflect.DeepEqual(result.Verify, []string{"real.test:1"}) {
		t.Fatalf("merge = %+v, %v", result, err)
	}
	assertBookPeers(t, b, nil)
}

func TestBookReplacementsSucceedAtCapacity(t *testing.T) {
	b := openReconcileBook(t, bookTestDir(t), nil)
	peers := make(map[string]string, MaxPeers-1)
	for i := 1; i < MaxPeers; i++ {
		peers[fmt.Sprintf("p%04d.test:1", i)] = testNodeID(i)
	}
	for address, id := range peers {
		if _, err := b.merge(Message{Address: address, NodeID: id}, address, nil); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := b.merge(Message{Address: "p0000.test:1", NodeID: testNodeID(0), Peers: peers}, "p0000.test:1", nil); err != nil || result.Rejected != 0 {
		t.Fatalf("filling Merge = %+v, %v", result, err)
	}
	if result, err := b.merge(Message{Address: "p0000.test:1", NodeID: testNodeID(MaxPeers)}, "p0000.test:1", nil); err != nil || result.Rejected != 0 {
		t.Fatalf("same-address replacement = %+v, %v", result, err)
	}
	// Retaining the old bootstrap address must not consume the slot needed
	// by its verified identity at the new address.
	if _, err := b.AddSeeds([]string{"p0001.test:1"}); err != nil {
		t.Fatal(err)
	}
	if result, err := b.merge(Message{Address: "moved.test:1", NodeID: testNodeID(1)}, "moved.test:1", nil); err != nil || result.Rejected != 0 {
		t.Fatalf("identity move = %+v, %v", result, err)
	}
	_, view := b.observe()
	if result, err := b.merge(Message{Address: "real.test:1", NodeID: testNodeID(MaxPeers + 1)}, "p0002.test:1", &view); err != nil || result.Rejected != 0 {
		t.Fatalf("probe alias replacement = %+v, %v", result, err)
	}
	got := b.Snapshot().Peers
	if len(got) != MaxPeers-1 || got["p0000.test:1"] != testNodeID(MaxPeers) || got["moved.test:1"] != testNodeID(1) ||
		got["real.test:1"] != "" || got["p0001.test:1"] != "" || got["p0002.test:1"] != "" {
		t.Fatalf("replacements at capacity: count %d, p0000 %q, moved %q, real %q, p0001 %q, p0002 %q", len(got),
			got["p0000.test:1"], got["moved.test:1"], got["real.test:1"], got["p0001.test:1"], got["p0002.test:1"])
	}
}

func TestBookSelfCollisions(t *testing.T) {
	dir := bookTestDir(t)
	b := openReconcileBook(t, dir, map[string]string{"peer.test:1": testNodeID(1)})
	self := b.Snapshot().NodeID
	before, err := os.ReadFile(filepath.Join(dir, bookStateName))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{Address: "self.test:1", NodeID: testNodeID(2)},
		{Address: "other.test:1", NodeID: self, Peers: map[string]string{"new.test:1": testNodeID(3)}},
	} {
		if _, err := b.merge(message, "", nil); !errors.Is(err, errIdentityCollision) {
			t.Fatalf("Merge(%+v) error = %v, want identity collision", message, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, bookStateName))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("collision changed persisted state: %v", err)
	}
	assertBookPeers(t, b, map[string]string{"peer.test:1": testNodeID(1)})

	result, err := b.merge(Message{Address: "sender.test:1", NodeID: testNodeID(4), Peers: map[string]string{"elsewhere.test:1": self}}, "sender.test:1", nil)
	if err != nil || !reflect.DeepEqual(result.Verify, []string{"elsewhere.test:1"}) {
		t.Fatalf("gossip of own identity = %+v, %v", result, err)
	}
	assertBookPeers(t, b, map[string]string{"peer.test:1": testNodeID(1), "sender.test:1": testNodeID(4)})
}

func TestBookLateResponseCannotRollBack(t *testing.T) {
	for name, tc := range map[string]struct {
		newer    []Message
		response Message
		want     map[string]string
	}{
		"newer move": {
			newer:    []Message{{Address: "b.test:1", NodeID: testNodeID(1)}},
			response: Message{Address: "a.test:1", NodeID: testNodeID(1)},
			want:     map[string]string{"b.test:1": testNodeID(1)},
		},
		"ABA replacement": {
			newer:    []Message{{Address: "a.test:1", NodeID: testNodeID(2)}, {Address: "a.test:1", NodeID: testNodeID(1)}},
			response: Message{Address: "b.test:1", NodeID: testNodeID(2)},
			want:     map[string]string{"a.test:1": testNodeID(1)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := openReconcileBook(t, bookTestDir(t), map[string]string{"a.test:1": testNodeID(1)})
			_, view := b.observe()
			for _, message := range tc.newer {
				if _, err := b.merge(message, message.Address, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.merge(tc.response, "a.test:1", &view); !errors.Is(err, errStaleObservation) {
				t.Fatalf("late merge error = %v, want stale observation", err)
			}
			assertBookPeers(t, b, tc.want)
		})
	}
}

func TestBookUnrelatedAdditionsKeepProbeCurrent(t *testing.T) {
	b := openReconcileBook(t, bookTestDir(t), map[string]string{"a.test:1": testNodeID(1)})
	_, view := b.observe()
	if _, err := b.merge(Message{Address: "u.test:1", NodeID: testNodeID(5)}, "u.test:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddSeeds([]string{"seed.test:1"}); err != nil {
		t.Fatal(err)
	}
	response := Message{Address: "a.test:1", NodeID: testNodeID(1), Peers: map[string]string{"c.test:1": testNodeID(3)}}
	if result, err := b.merge(response, "a.test:1", &view); err != nil || !reflect.DeepEqual(result.Verify, []string{"c.test:1"}) {
		t.Fatalf("merge = %+v, %v", result, err)
	}
	assertBookPeers(t, b, map[string]string{"a.test:1": testNodeID(1), "u.test:1": testNodeID(5)})
	if got := b.Addresses(); !reflect.DeepEqual(got, []string{"a.test:1", "seed.test:1", "u.test:1"}) {
		t.Fatalf("Addresses = %v", got)
	}
}

func TestBookFailedReplacementPreservesMemoryAndFile(t *testing.T) {
	for name, message := range map[string]Message{
		"identity move":     {Address: "new.test:1", NodeID: testNodeID(1)},
		"same address":      {Address: "old.test:1", NodeID: testNodeID(2)},
		"third-party probe": {Address: "real.test:1", NodeID: testNodeID(3)},
	} {
		t.Run(name, func(t *testing.T) {
			dir := bookTestDir(t)
			b := openReconcileBook(t, dir, map[string]string{"old.test:1": testNodeID(1)})
			before, err := os.ReadFile(filepath.Join(dir, bookStateName))
			if err != nil {
				t.Fatal(err)
			}
			_, view := b.observe()
			b.write = func(string, map[string]bookRecord) (bool, error) { return false, syscall.ENOSPC }
			if _, err := b.merge(message, "old.test:1", &view); err == nil {
				t.Fatal("merge succeeded")
			}
			after, err := os.ReadFile(filepath.Join(dir, bookStateName))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed replacement changed persisted state: %v", err)
			}
			assertBookPeers(t, b, map[string]string{"old.test:1": testNodeID(1)})
		})
	}
}

func TestBookReplacementDirectorySyncFailureRetainsAndRetries(t *testing.T) {
	dir := bookTestDir(t)
	b := openReconcileBook(t, dir, map[string]string{"old.test:1": testNodeID(1)})
	_, view := b.observe()
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
	message := Message{Address: "new.test:1", NodeID: testNodeID(1)}
	if _, err := b.merge(message, message.Address, nil); err == nil {
		t.Fatal("acknowledged a replacement whose directory sync failed")
	}
	want := map[string]string{"new.test:1": testNodeID(1)}
	assertBookPeers(t, b, want)
	if got, err := loadBookState(dir); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("disk disagrees with published replacement: %v, %v", got, err)
	}
	// The published replacement must invalidate observations taken before it.
	if _, err := b.merge(Message{Address: "old.test:1", NodeID: testNodeID(1)}, "old.test:1", &view); !errors.Is(err, errStaleObservation) {
		t.Fatalf("pre-replacement response error = %v, want stale observation", err)
	}
	if _, err := b.merge(message, message.Address, nil); err != nil || syncCalls != 2 {
		t.Fatalf("identical exchange did not retry persistence: calls=%d, error=%v", syncCalls, err)
	}
	assertBookPeers(t, b, want)
}

func BenchmarkBookMergeFull(b *testing.B) {
	book, err := OpenBook(context.Background(), filepath.Join(b.TempDir(), "book"), "self.test:1", nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer book.Close()
	message := Message{Address: "sender.test:1", NodeID: testNodeID(MaxPeers), Peers: make(map[string]string)}
	for i := range MaxPeers - 1 {
		message.Peers[fmt.Sprintf("peer-%04d.test:1", i)] = testNodeID(i)
	}
	for address, id := range message.Peers {
		if _, err := book.merge(Message{Address: address, NodeID: id}, address, nil); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := book.merge(message, message.Address, nil); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := book.merge(message, message.Address, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// openReconcileBook opens a book for self.test:1 holding peers, each sent
// directly by its own address so that no gossip conflict rules apply.
func openReconcileBook(t *testing.T, dir string, peers map[string]string) *Book {
	t.Helper()
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	for address, id := range peers {
		if _, err := b.merge(Message{Address: address, NodeID: id}, address, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertBookPeers(t, b, peers)
	return b
}

func assertBookPeers(t *testing.T, b *Book, want map[string]string) {
	t.Helper()
	if got := b.Snapshot().Peers; !maps.Equal(got, want) {
		t.Fatalf("peers = %v, want %v", got, want)
	}
}
