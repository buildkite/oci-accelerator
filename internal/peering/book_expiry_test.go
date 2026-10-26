package peering

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestBookInboundCannotConfirmReplaceOrRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := openReconcileBook(t, bookTestDir(t), map[string]string{"known.test:1": testNodeID(1)})
		time.Sleep(20 * time.Minute)
		for _, tc := range []struct {
			address string
			id      string
			verify  []string
		}{
			{"known.test:1", testNodeID(1), nil},
			{"known.test:1", testNodeID(2), []string{"known.test:1"}},
			{"unknown.test:1", testNodeID(1), []string{"unknown.test:1"}},
		} {
			result, err := b.merge(Message{Address: tc.address, NodeID: tc.id}, "", nil)
			if err != nil || !reflect.DeepEqual(result.Verify, tc.verify) || len(result.Added) != 0 {
				t.Fatalf("inbound merge = %+v, %v", result, err)
			}
			assertBookPeers(t, b, map[string]string{"known.test:1": testNodeID(1)})
		}
		time.Sleep(10 * time.Minute)
		assertBookPeers(t, b, nil)
		result, err := b.merge(Message{Address: "known.test:1", NodeID: testNodeID(1)}, "", nil)
		if err != nil || !reflect.DeepEqual(result.Verify, []string{"known.test:1"}) {
			t.Fatalf("expired inbound = %+v, %v", result, err)
		}
	})
}

func TestBookRenewsUnchangedMemberAcrossExpiry(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(map[bool]string{false: "member", true: "configured seed"}[seed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := Message{Address: "peer.test:1", NodeID: testNodeID(1)}
				b := openReconcileBook(t, bookTestDir(t), map[string]string{peer.Address: peer.NodeID})
				if seed {
					if _, err := b.AddSeeds([]string{peer.Address}); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(29*time.Minute + 59*time.Second)
				_, before := b.observe()
				time.Sleep(2 * time.Second)
				assertBookPeers(t, b, nil)
				if _, err := b.merge(peer, peer.Address, &before); err != nil {
					t.Fatalf("unchanged member renewal rejected: %v", err)
				}
				if err := b.expire(time.Now()); err != nil {
					t.Fatal(err)
				}
				time.Sleep(29*time.Minute + 59*time.Second)
				assertBookPeers(t, b, map[string]string{peer.Address: peer.NodeID})
				time.Sleep(time.Second)
				assertBookPeers(t, b, nil)
			})
		})
	}
}

func TestBookAliasAndGossipCannotRefreshMembership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := openReconcileBook(t, bookTestDir(t), map[string]string{"real.test:1": testNodeID(1), "alias.test:1": testNodeID(2)})
		if _, err := b.AddSeeds([]string{"alias.test:1"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Minute)
		_, view := b.observe()
		result, err := b.merge(Message{Address: "real.test:1", NodeID: testNodeID(1)}, "alias.test:1", &view)
		if err != nil || !reflect.DeepEqual(result.Verify, []string{"real.test:1"}) || len(result.Added) != 0 {
			t.Fatalf("alias response = %+v, %v", result, err)
		}
		if _, err := b.merge(Message{Address: "sender.test:1", NodeID: testNodeID(3), Peers: map[string]string{"real.test:1": testNodeID(1)}}, "sender.test:1", nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Minute)
		assertBookPeers(t, b, map[string]string{"sender.test:1": testNodeID(3)})
		if err := b.expire(time.Now()); err != nil {
			t.Fatal(err)
		}
		_, view = b.observe()
		if !maps.Equal(view.peers, map[string]string{"alias.test:1": "", "sender.test:1": testNodeID(3)}) {
			t.Fatalf("alias retry state = %v", view.peers)
		}
	})
}

func TestBookExpiryRefreshReopenAndSeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := bookTestDir(t)
		b := openReconcileBook(t, dir, map[string]string{"peer.test:1": testNodeID(1), "seed.test:1": testNodeID(2)})
		if _, err := b.AddSeeds([]string{"seed.test:1", "unresolved.test:1"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Minute)
		if _, err := b.merge(Message{Address: "peer.test:1", NodeID: testNodeID(1)}, "peer.test:1", nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Minute)
		assertBookPeers(t, b, map[string]string{"peer.test:1": testNodeID(1)})
		if err := b.expire(time.Now()); err != nil {
			t.Fatal(err)
		}
		_, view := b.observe()
		if !maps.Equal(view.peers, map[string]string{"peer.test:1": testNodeID(1), "seed.test:1": "", "unresolved.test:1": ""}) {
			t.Fatalf("scheduler view = %v", view.peers)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		if _, err := b.AddSeeds([]string{"seed.test:1"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Minute)
		assertBookPeers(t, b, nil)
		if err := b.expire(time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, err := loadBookState(dir); err != nil || len(got) != 0 {
			t.Fatalf("expired state = %v, %v", got, err)
		}
		time.Sleep(24 * time.Hour)
		if err := b.expire(time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := b.Addresses(); !reflect.DeepEqual(got, []string{"seed.test:1"}) {
			t.Fatalf("configured seed lost: %v", got)
		}
		if _, err := b.merge(Message{Address: "seed.test:1", NodeID: testNodeID(2)}, "seed.test:1", nil); err != nil {
			t.Fatal(err)
		}
		assertBookPeers(t, b, map[string]string{"seed.test:1": testNodeID(2)})
	})
}

func TestBookExpiryPersistenceFailures(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before rename", true: "directory sync"}[published], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := bookTestDir(t)
				b := openReconcileBook(t, dir, map[string]string{"peer.test:1": testNodeID(1)})
				time.Sleep(30 * time.Minute)
				b.write = func(dir string, records map[string]bookRecord) (bool, error) {
					if published {
						return replaceBookState(dir, records, func(string) error { return syscall.EIO })
					}
					return false, syscall.ENOSPC
				}
				if err := b.expire(time.Now()); err == nil {
					t.Fatal("expiry acknowledged failed persistence")
				}
				assertBookPeers(t, b, nil)
				peers, err := loadBookState(dir)
				if err != nil || (len(peers) == 0) != published {
					t.Fatalf("failed expiry disk state: %v, %v", peers, err)
				}
				b.write = writeBookState
				if err := b.expire(time.Now()); err != nil {
					t.Fatal(err)
				}
				if peers, err := loadBookState(dir); err != nil || len(peers) != 0 {
					t.Fatalf("expiry retry did not persist: %v, %v", peers, err)
				}
			})
		})
	}
}

func TestBookRenewalCheckpointsAndConservativeRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := bookTestDir(t)
		peer := Message{Address: "peer.test:1", NodeID: testNodeID(1)}
		b := openReconcileBook(t, dir, map[string]string{peer.Address: peer.NodeID})
		writes := 0
		b.write = func(dir string, records map[string]bookRecord) (bool, error) {
			writes++
			return writeBookState(dir, records)
		}
		renew := func() {
			t.Helper()
			if _, err := b.merge(peer, peer.Address, nil); err != nil {
				t.Fatal(err)
			}
		}
		for range 14 {
			time.Sleep(30 * time.Second)
			renew()
		}
		time.Sleep(29 * time.Second)
		renew()
		if writes != 0 {
			t.Fatalf("renewals below checkpoint threshold wrote %d times", writes)
		}
		time.Sleep(time.Second)
		renew()
		if writes != 1 {
			t.Fatalf("renewal at 7m30s wrote %d times, want 1", writes)
		}
		checkpoint := time.Now()
		time.Sleep(time.Minute)
		renew()
		if writes != 1 {
			t.Fatal("renewal immediately after checkpoint wrote again")
		}
		records, err := loadBookRecords(dir)
		if err != nil || !records[peer.Address].LastSuccess.Equal(checkpoint) {
			t.Fatalf("saved checkpoint = %v, %v", records, err)
		}
		time.Sleep(29 * time.Minute)
		// Live freshness includes the unsaved renewal one minute later.
		assertBookPeers(t, b, map[string]string{peer.Address: peer.NodeID})
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		b, err = OpenBook(context.Background(), dir, "self.test:1", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		assertBookPeers(t, b, nil)
	})
}

func TestBookMembershipChangeCheckpointsRecentRenewals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := bookTestDir(t)
		b := openReconcileBook(t, dir, map[string]string{"peer.test:1": testNodeID(1)})
		time.Sleep(time.Minute)
		renewed := time.Now()
		if _, err := b.merge(Message{Address: "peer.test:1", NodeID: testNodeID(1)}, "peer.test:1", nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		if _, err := b.merge(Message{Address: "new.test:1", NodeID: testNodeID(2)}, "new.test:1", nil); err != nil {
			t.Fatal(err)
		}
		records, err := loadBookRecords(dir)
		if err != nil || len(records) != 2 || records["new.test:1"].NodeID != testNodeID(2) ||
			!records["peer.test:1"].LastSuccess.Equal(renewed) {
			t.Fatalf("membership change did not save current freshness: %v, %v", records, err)
		}
	})
}

func TestBookRenewalCheckpointRetriesPersistenceFailures(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before rename", true: "directory sync"}[published], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := openReconcileBook(t, bookTestDir(t), map[string]string{"peer.test:1": testNodeID(1)})
				time.Sleep(7*time.Minute + 30*time.Second)
				writes := 0
				b.write = func(dir string, records map[string]bookRecord) (bool, error) {
					writes++
					if writes == 1 {
						if published {
							return replaceBookState(dir, records, func(string) error { return syscall.EIO })
						}
						return false, syscall.ENOSPC
					}
					return writeBookState(dir, records)
				}
				peer := Message{Address: "peer.test:1", NodeID: testNodeID(1)}
				if _, err := b.merge(peer, peer.Address, nil); err == nil {
					t.Fatal("failed checkpoint acknowledged")
				}
				if _, err := b.merge(peer, peer.Address, nil); err != nil || writes != 2 {
					t.Fatalf("checkpoint retry: writes=%d error=%v", writes, err)
				}
				if _, err := b.merge(peer, peer.Address, nil); err != nil || writes != 2 {
					t.Fatalf("checkpoint did not resume coalescing: writes=%d error=%v", writes, err)
				}
			})
		})
	}
}

func TestBookFailedRefreshDoesNotExtendLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := bookTestDir(t)
		b := openReconcileBook(t, dir, map[string]string{"peer.test:1": testNodeID(1)})
		time.Sleep(20 * time.Minute)
		b.write = func(string, map[string]bookRecord) (bool, error) { return false, syscall.ENOSPC }
		if _, err := b.merge(Message{Address: "peer.test:1", NodeID: testNodeID(1)}, "peer.test:1", nil); err == nil {
			t.Fatal("failed refresh acknowledged")
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		_, before := b.observe()
		time.Sleep(10 * time.Minute)
		assertBookPeers(t, b, nil)
		if err := b.expire(time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.merge(Message{Address: "peer.test:1", NodeID: testNodeID(1)}, "peer.test:1", &before); !errors.Is(err, errStaleObservation) {
			t.Fatalf("pre-expiry response = %v, want stale observation", err)
		}
	})
}
