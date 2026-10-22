package peering

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestDiscoveryStaleResultPreservesFailures(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate", true: "member"}[member], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				release := make(chan struct{})
				peer := Message{Address: "peer.test:1", NodeID: testNodeID(1)}
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					if r.URL.Host != peer.Address {
						return nil, context.DeadlineExceeded
					}
					if calls.Add(1) == 3 {
						select {
						case <-release:
							return messageResponse(peer), nil
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return nil, context.DeadlineExceeded
				})
				var logs bytes.Buffer
				d.logger = slog.New(slog.NewTextHandler(&logs, nil))
				if member {
					if _, err := d.book.merge(peer, peer.Address, nil); err != nil {
						t.Fatal(err)
					}
				} else if response := cooldownRequest(d, peer); response.Code != http.StatusOK {
					t.Fatalf("introduction status = %d", response.Code)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				synctest.Wait()
				for range 109 {
					if calls.Load() == 3 {
						break
					}
					time.Sleep(time.Second)
					synctest.Wait()
				}
				if calls.Load() != 3 {
					t.Fatal("third exchange did not start")
				}
				// A replacement during the request invalidates its observation.
				for _, id := range []string{testNodeID(2), testNodeID(3)} {
					if _, err := d.book.merge(Message{Address: "other.test:1", NodeID: id}, "other.test:1", nil); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				synctest.Wait()
				if strings.Contains(logs.String(), "peer exchange recovered") {
					t.Fatal("stale response reported recovery")
				}
				time.Sleep(47 * time.Second)
				synctest.Wait()
				if calls.Load() != 3 {
					t.Fatal("stale response reset failure backoff")
				}
				time.Sleep(26 * time.Second)
				synctest.Wait()
				if calls.Load() != 4 {
					t.Fatalf("attempts = %d, want next real failure", calls.Load())
				}
				if !member {
					for range 120 {
						time.Sleep(time.Second)
						if response := cooldownRequest(d, peer); response.Code != http.StatusOK {
							t.Fatalf("reintroduction status = %d", response.Code)
						}
						synctest.Wait()
					}
					if calls.Load() != 4 {
						t.Fatal("stale response replenished candidate retry budget")
					}
				}
			})
		})
	}
}

func TestDiscoveryCandidateBudgetSurvivesRepeatedGossip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, context.DeadlineExceeded
		})
		gossip := Message{Address: d.book.self, NodeID: d.book.nodeID, Peers: map[string]string{"dead.test:1": testNodeID(1)}}
		introduce := func() {
			t.Helper()
			if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
				t.Fatalf("gossip status = %d", response.Code)
			}
			synctest.Wait()
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		introduce()
		if calls.Load() != 1 || len(d.book.Snapshot().Peers) != 0 {
			t.Fatal("candidate was not probed once or was advertised without contact")
		}
		for range 109 {
			time.Sleep(time.Second)
			introduce()
		}
		if calls.Load() != 3 {
			t.Fatalf("attempts = %d, want three despite repeated gossip", calls.Load())
		}
		for range 60 {
			time.Sleep(time.Second)
			introduce()
		}
		if calls.Load() != 3 {
			t.Fatalf("gossip bypassed exhausted budget: %d attempts", calls.Load())
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatalf("forgotten candidate retried without a new introduction: %d", calls.Load())
		}
		introduce()
		if calls.Load() != 4 {
			t.Fatalf("new introduction after cooldown did not retry: %d", calls.Load())
		}
	})
}

func TestDiscoveryExpiresPeerAndVerifiesReturningInboundSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var reachable atomic.Bool
		peer := Message{Address: "peer.test:1", NodeID: testNodeID(1)}
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if !reachable.Load() {
				return nil, context.DeadlineExceeded
			}
			return messageResponse(peer), nil
		})
		if _, err := d.book.merge(peer, peer.Address, nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		// Inbound contact cannot renew membership or postpone probes.
		for range 31 {
			time.Sleep(time.Minute)
			if response := cooldownRequest(d, peer); response.Code != http.StatusOK {
				t.Fatalf("inbound status = %d", response.Code)
			}
			synctest.Wait()
		}
		if len(d.book.Snapshot().Peers) != 0 {
			t.Fatal("inbound traffic kept unreachable membership alive")
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		before := calls.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != before {
			t.Fatal("forgotten peer still retried without introduction")
		}
		reachable.Store(true)
		if response := cooldownRequest(d, peer); response.Code != http.StatusOK {
			t.Fatalf("returning sender status = %d", response.Code)
		}
		synctest.Wait()
		if got, want := d.book.Snapshot().Peers, map[string]string{peer.Address: peer.NodeID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("returning peer membership = %v, want %v", got, want)
		}
		if calls.Load() != before+1 {
			t.Fatal("returning peer was not probed back")
		}
	})
}

func TestDiscoveryKeepsRetryingConfiguredSeedAfterExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, context.DeadlineExceeded
		})
		if _, err := d.book.AddSeeds([]string{"seed.test:1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.book.merge(Message{Address: "seed.test:1", NodeID: testNodeID(1)}, "seed.test:1", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		before := calls.Load()
		time.Sleep(7 * time.Minute)
		synctest.Wait()
		if calls.Load() <= before || len(d.book.Snapshot().Peers) != 0 {
			t.Fatal("expired seed stopped retrying or remained advertised")
		}
	})
}

func TestDiscoveryMigratesUnidentifiedAddressesAsBoundedCandidates(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"peers":["legacy.test:1"]}`,
		`{"version":2,"peers":{"legacy.test:1":""}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := cooldownDiscovery(t, nil)
				dir := d.book.dir
				if err := d.book.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, bookStateName), []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				book, err := OpenBook(dir, "b.test:2")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { book.Close() })
				var calls atomic.Int32
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, context.DeadlineExceeded
				})}
				d = NewDiscovery(book, client, d.logger, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				time.Sleep(5 * time.Minute)
				synctest.Wait()
				if calls.Load() != 3 || len(book.Snapshot().Peers) != 0 {
					t.Fatalf("legacy candidate: attempts=%d members=%v", calls.Load(), book.Snapshot().Peers)
				}
			})
		})
	}
}
