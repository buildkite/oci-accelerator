package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

func TestDiscoveryVerificationResponseDecidesConflictingClaim(t *testing.T) {
	id1, id3, id5, id9 := testNodeID(1), testNodeID(3), testNodeID(5), testNodeID(9)
	original := map[string]string{"a.test:5": id5, "x.test:1": id1}
	reply := func(m Message) func() (*http.Response, error) {
		return func() (*http.Response, error) { return messageResponse(m), nil }
	}
	for _, tc := range []struct {
		name   string
		claim  map[string]string
		probe  string
		reply  func() (*http.Response, error) // nil cancels Run during the probe
		result map[string]string
	}{
		{"address claim confirmed", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "x.test:1", NodeID: id9}), map[string]string{"a.test:5": id5, "x.test:1": id9}},
		{"address claim refuted", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "x.test:1", NodeID: id1}), original},
		{"address answered by third identity alias", map[string]string{"x.test:1": id9}, "x.test:1",
			reply(Message{Address: "y.test:7", NodeID: id3}), map[string]string{"a.test:5": id5, "y.test:7": id3}},
		{"identity claim confirmed", map[string]string{"z.test:9": id1}, "z.test:9",
			reply(Message{Address: "z.test:9", NodeID: id1}), map[string]string{"a.test:5": id5, "z.test:9": id1}},
		{"identity claim answered by third identity alias", map[string]string{"z.test:9": id1}, "z.test:9",
			reply(Message{Address: "y.test:7", NodeID: id3}), map[string]string{"a.test:5": id5, "x.test:1": id1, "y.test:7": id3}},
		{"identity probe fails", map[string]string{"z.test:9": id1}, "z.test:9",
			func() (*http.Response, error) { return nil, context.DeadlineExceeded }, original},
		{"address probe cancelled", map[string]string{"x.test:1": id9}, "x.test:1", nil, original},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				release := make(chan struct{})
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					switch r.URL.Host {
					case tc.probe:
						select {
						case <-release:
							return tc.reply()
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					case "x.test:1":
						// Identity relocation is attempted only after the old address fails.
						return nil, context.DeadlineExceeded
					case "y.test:7":
						return messageResponse(Message{Address: "y.test:7", NodeID: id3}), nil
					case "a.test:5":
						return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
					}
					return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
				})
				confirmTestPeers(t, d.book, original)
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: tc.claim}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
					t.Fatalf("gossip changed book before verification: %v", got)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { d.Run(ctx); close(done) }()
				synctest.Wait()
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
					t.Fatalf("book changed before probe answered: %v", got)
				}
				if log.count(tc.probe) != 1 || log.count("a.test:5") != 1 {
					t.Fatalf("requests = %v, want one probe and one regular sender exchange", log.all())
				}
				if tc.reply == nil {
					cancel()
					<-done
				} else {
					close(release)
					synctest.Wait()
				}
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, tc.result) {
					t.Fatalf("book = %v, want %v", got, tc.result)
				}
				if log.count(tc.probe) != 1 {
					t.Fatalf("probe requests = %d, want 1", log.count(tc.probe))
				}
			})
		})
	}
}

func TestDiscoveryGossipRetainsWorkingAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current Message
		want    string
		probes  int
	}{
		{"same identity", Message{Address: "x.test:1", NodeID: testNodeID(1)}, "x.test:1", 0},
		{"unreachable", Message{}, "z.test:9", 1},
		{"different identity", Message{Address: "x.test:1", NodeID: testNodeID(2)}, "z.test:9", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					switch r.URL.Host {
					case "x.test:1":
						if tc.current.NodeID == "" {
							return nil, context.DeadlineExceeded
						}
						return messageResponse(tc.current), nil
					case "z.test:9":
						return messageResponse(Message{Address: "z.test:9", NodeID: testNodeID(1)}), nil
					case "a.test:5":
						return messageResponse(Message{Address: "a.test:5", NodeID: testNodeID(5)}), nil
					}
					return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
				})
				confirmTestPeers(t, d.book, map[string]string{"a.test:5": testNodeID(5), "x.test:1": testNodeID(1)})
				gossip := Message{Address: "a.test:5", NodeID: testNodeID(5), Peers: map[string]string{"z.test:9": testNodeID(1)}}
				if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				synctest.Wait()
				want := map[string]string{"a.test:5": testNodeID(5), tc.want: testNodeID(1)}
				if tc.current.NodeID == testNodeID(2) {
					want["x.test:1"] = testNodeID(2)
				}
				if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
					t.Fatalf("book = %v, want %v", got, want)
				}
				if log.count("x.test:1") != 1 || log.count("z.test:9") != tc.probes {
					t.Fatalf("requests = %v, want one current-address check and %d new-address probes", log.all(), tc.probes)
				}
				// Repeated gossip does not bypass the claimed address's cooldown.
				for range 3 {
					if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
						t.Fatalf("repeated gossip status = %d", response.Code)
					}
				}
				time.Sleep(23 * time.Second)
				synctest.Wait()
				if log.count("x.test:1") != 1 || log.count("z.test:9") != tc.probes {
					t.Fatalf("gossip bypassed cooldown: %v", log.all())
				}
				if tc.want == "x.test:1" {
					// Once the cooldown expires, fresh gossip still cannot displace A.
					time.Sleep(14 * time.Second)
					synctest.Wait()
					if log.count("z.test:9") != 0 || !reflect.DeepEqual(d.book.Snapshot().Peers, want) {
						t.Fatalf("repeated gossip displaced working address: requests=%v book=%v", log.all(), d.book.Snapshot().Peers)
					}
				}
			})
		})
	}
}

func TestDiscoverySuppressedCandidatePreservesFailureCooldown(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pendingCheck bool
		lateFailure  bool
	}{
		{"after current check", false, false},
		{"before current check", true, false},
		{"before candidate failure", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				release := make(chan struct{})
				if !tc.pendingCheck {
					close(release)
				}
				releaseCandidate := make(chan struct{})
				if !tc.lateFailure {
					close(releaseCandidate)
				}
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					if r.URL.Host == "x.test:1" {
						select {
						case <-release:
							return messageResponse(Message{Address: "x.test:1", NodeID: testNodeID(1)}), nil
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					select {
					case <-releaseCandidate:
						return nil, context.DeadlineExceeded
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				})
				confirmTestPeers(t, d.book, map[string]string{"x.test:1": testNodeID(1)})
				gossip := Message{Address: d.book.self, NodeID: d.book.nodeID, Peers: map[string]string{"z.test:9": testNodeID(2)}}
				if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
					t.Fatalf("initial gossip status = %d", response.Code)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				synctest.Wait()
				if log.count("z.test:9") != 1 {
					t.Fatalf("initial candidate attempts = %d, want 1", log.count("z.test:9"))
				}
				// A conflicting claim suppresses B, but must not erase B's failure history.
				gossip.Peers["z.test:9"] = testNodeID(1)
				if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
					t.Fatalf("conflicting gossip status = %d", response.Code)
				}
				if tc.pendingCheck {
					close(release)
				}
				synctest.Wait()
				if tc.lateFailure {
					close(releaseCandidate)
					synctest.Wait()
				}
				time.Sleep(37 * time.Second)
				synctest.Wait()
				// Changing the claim again cannot obtain a fresh retry budget before cooldown.
				gossip.Peers["z.test:9"] = testNodeID(2)
				for range 3 {
					if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
						t.Fatalf("reintroduced gossip status = %d", response.Code)
					}
					time.Sleep(37 * time.Second)
					synctest.Wait()
					if log.count("z.test:9") != 1 || d.book.Snapshot().Peers["x.test:1"] != testNodeID(1) {
						t.Fatalf("suppression reset failed candidate: requests=%v book=%v", log.all(), d.book.Snapshot().Peers)
					}
				}
			})
		})
	}
}

func TestDiscoveryCandidateCannotReplaceRecoveredMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log requestLog
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			switch r.URL.Host {
			case "x.test:1":
				if log.count(r.URL.Host) == 1 {
					return nil, context.DeadlineExceeded
				}
			case "z.test:9":
				select {
				case <-time.After(2 * time.Second):
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			default:
				return nil, context.DeadlineExceeded
			}
			return messageResponse(Message{Address: r.URL.Host, NodeID: testNodeID(1)}), nil
		})
		d.client.Timeout = 5 * time.Second
		confirmTestPeers(t, d.book, map[string]string{"x.test:1": testNodeID(1)})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		d.mu.Lock()
		retry := d.known["x.test:1"].next
		d.mu.Unlock()
		// B starts before A's retry and answers after A has recovered, all
		// within the production request deadline.
		time.Sleep(time.Until(retry) - time.Second)
		if response := cooldownRequest(d, Message{Address: d.book.self, NodeID: d.book.nodeID, Peers: map[string]string{"z.test:9": testNodeID(1)}}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		want := map[string]string{"x.test:1": testNodeID(1)}
		if !reflect.DeepEqual(d.book.Snapshot().Peers, want) || log.count("x.test:1") != 2 || log.count("z.test:9") != 1 {
			t.Fatalf("candidate displaced recovered member: book=%v requests=%v", d.book.Snapshot().Peers, log.all())
		}
	})
}

func TestDiscoveryConflictingGossipDoesNotBlockRetainedPeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log requestLog
		peers := map[string]string{"x.test:1": testNodeID(1), "y.test:2": testNodeID(2)}
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			if id := peers[r.URL.Host]; id != "" {
				return messageResponse(Message{Address: r.URL.Host, NodeID: id}), nil
			}
			return nil, context.DeadlineExceeded
		})
		confirmTestPeers(t, d.book, peers)
		confirmTestPeers(t, d.book, map[string]string{"a.test:5": testNodeID(5)})
		swapped := map[string]string{"x.test:1": testNodeID(2), "y.test:2": testNodeID(1)}
		if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: testNodeID(5), Peers: swapped}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		time.Sleep(37 * time.Second)
		synctest.Wait()
		want := map[string]string{"a.test:5": testNodeID(5), "x.test:1": testNodeID(1), "y.test:2": testNodeID(2)}
		if !reflect.DeepEqual(d.book.Snapshot().Peers, want) || log.count("x.test:1") != 2 || log.count("y.test:2") != 2 {
			t.Fatalf("cyclic gossip blocked discovery: book=%v requests=%v", d.book.Snapshot().Peers, log.all())
		}
	})
}

func TestDiscoveryGossipRechecksSupersededIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log requestLog
		var mu sync.Mutex
		id := testNodeID(1)
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			if r.URL.Host != "x.test:1" && r.URL.Host != "z.test:9" {
				return nil, context.DeadlineExceeded
			}
			mu.Lock()
			currentID := id
			mu.Unlock()
			return messageResponse(Message{Address: r.URL.Host, NodeID: currentID}), nil
		})
		confirmTestPeers(t, d.book, map[string]string{"x.test:1": testNodeID(1)})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		mu.Lock()
		id = testNodeID(2)
		mu.Unlock()
		// Another verified observation replaces the binding during cooldown.
		confirmTestPeers(t, d.book, map[string]string{"x.test:1": testNodeID(2)})
		if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: testNodeID(5), Peers: map[string]string{"z.test:9": testNodeID(2)}}); response.Code != http.StatusOK {
			t.Fatalf("inbound status = %d", response.Code)
		}
		synctest.Wait()
		if log.count("x.test:1") != 1 || log.count("z.test:9") != 0 {
			t.Fatalf("superseded confirmation bypassed cooldown: %v", log.all())
		}
		time.Sleep(37 * time.Second)
		synctest.Wait()
		if log.count("x.test:1") != 2 || log.count("z.test:9") != 0 || d.book.Snapshot().Peers["x.test:1"] != testNodeID(2) {
			t.Fatalf("new identity was not checked before replacement: book=%v requests=%v", d.book.Snapshot().Peers, log.all())
		}
	})
}

func TestDiscoveryGossipAliasesShareCurrentCheck(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "multiple aliases"
		if full {
			name = "inbound alias at membership capacity"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				release := make(chan struct{})
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					if r.URL.Host != "x.test:1" {
						return nil, context.DeadlineExceeded
					}
					select {
					case <-release:
						return messageResponse(Message{Address: "x.test:1", NodeID: testNodeID(1)}), nil
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				})
				peers := map[string]string{"x.test:1": testNodeID(1)}
				if full {
					for i := 2; i < MaxPeers; i++ {
						peers[fmt.Sprintf("filler-%d.test:1", i)] = testNodeID(i)
					}
				}
				peers["a.test:5"] = testNodeID(MaxPeers)
				confirmTestPeers(t, d.book, peers)
				aliases := make(map[string]string)
				for i := range 8 {
					aliases[fmt.Sprintf("alias-%d.test:9", i)] = testNodeID(1)
				}
				gossip := Message{Address: "a.test:5", NodeID: testNodeID(MaxPeers), Peers: aliases}
				if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
					t.Fatalf("alias gossip status = %d", response.Code)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				synctest.Wait()
				if full {
					// Inbound aliases remain candidates while the current member
					// is being checked, even when membership is full.
					if response := cooldownRequest(d, Message{Address: "alias-0.test:9", NodeID: testNodeID(1)}); response.Code != http.StatusOK {
						t.Fatalf("inbound alias status = %d", response.Code)
					}
				}
				close(release)
				synctest.Wait()
				if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
					t.Fatalf("repeated gossip status = %d", response.Code)
				}
				time.Sleep(23 * time.Second)
				synctest.Wait()
				if log.count("x.test:1") != 1 || d.book.Snapshot().Peers["x.test:1"] != testNodeID(1) {
					t.Fatalf("aliases did not share one check: book=%v requests=%v", d.book.Snapshot().Peers, log.all())
				}
				for address := range aliases {
					if log.count(address) != 0 {
						t.Fatalf("working address displaced by %s: requests=%v", address, log.all())
					}
				}
			})
		})
	}
}

func TestDiscoveryCurrentAddressCheckInterrupted(t *testing.T) {
	for _, cancelCheck := range []bool{false, true} {
		name := "newer verified move"
		if cancelCheck {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log requestLog
				release := make(chan struct{})
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					log.add(r.URL.Host)
					if r.URL.Host != "x.test:1" {
						id := testNodeID(5)
						if r.URL.Host == "w.test:4" {
							id = testNodeID(1)
						}
						return messageResponse(Message{Address: r.URL.Host, NodeID: id}), nil
					}
					select {
					case <-release:
						return messageResponse(Message{Address: "x.test:1", NodeID: testNodeID(1)}), nil
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				})
				confirmTestPeers(t, d.book, map[string]string{"a.test:5": testNodeID(5), "x.test:1": testNodeID(1)})
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: testNodeID(5), Peers: map[string]string{"z.test:9": testNodeID(1)}}); response.Code != http.StatusOK {
					t.Fatalf("inbound status = %d", response.Code)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { d.Run(ctx); close(done) }()
				synctest.Wait()
				want := map[string]string{"a.test:5": testNodeID(5), "x.test:1": testNodeID(1)}
				if cancelCheck {
					cancel()
					<-done
				} else {
					confirmTestPeers(t, d.book, map[string]string{"w.test:4": testNodeID(1)})
					want = map[string]string{"a.test:5": testNodeID(5), "w.test:4": testNodeID(1)}
					close(release)
					synctest.Wait()
				}
				if !reflect.DeepEqual(d.book.Snapshot().Peers, want) || log.count("x.test:1") != 1 || log.count("z.test:9") != 0 {
					t.Fatalf("interrupted check: book=%v requests=%v, want book=%v and no new-address probe", d.book.Snapshot().Peers, log.all(), want)
				}
			})
		})
	}
}

func TestDiscoveryVerificationDedupesAndRespectsBackoffWithoutDelayingInbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id1, id5, id9 := testNodeID(1), testNodeID(5), testNodeID(9)
		var log requestLog
		release := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			switch r.URL.Host {
			case "z.test:9":
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return nil, context.DeadlineExceeded
			case "x.test:1":
				return nil, context.DeadlineExceeded
			case "a.test:5":
				return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
			}
			return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
		})
		original := map[string]string{"a.test:5": id5, "x.test:1": id1}
		confirmTestPeers(t, d.book, original)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		if log.count("x.test:1") != 1 || log.count("a.test:5") != 1 {
			t.Fatalf("initial requests = %v", log.all())
		}
		// x failed at t=0, so its address conflict waits for x's backoff.
		// z shares that failed check instead of probing x again.
		gossip := Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"x.test:1": id9, "z.test:9": id1}}
		check := func(when string, x, z int) {
			t.Helper()
			synctest.Wait()
			if log.count("x.test:1") != x || log.count("z.test:9") != z {
				t.Fatalf("%s: requests = %v, want x=%d z=%d", when, log.all(), x, z)
			}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
				t.Fatalf("%s: book = %v, want %v", when, got, original)
			}
		}
		// A blocked inbound handler would deadlock the bubble here.
		for range 3 {
			if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
				t.Fatalf("gossip status = %d", response.Code)
			}
			check("while probe in flight", 1, 1)
		}
		close(release)
		check("after failed probe", 1, 1)
		time.Sleep(10 * time.Second)
		if response := cooldownRequest(d, gossip); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		// Both deadlines are at least 24s after t=0 and at most 36s.
		time.Sleep(13 * time.Second)
		check("during cooldown and backoff", 1, 1)
		time.Sleep(14 * time.Second)
		check("after cooldown and backoff", 2, 2)
	})
}

func TestDiscoveryVerificationsShareExchangeSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			log            requestLog
			mu             sync.Mutex
			active, maxAct int
		)
		release := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			mu.Lock()
			active++
			maxAct = max(maxAct, active)
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return nil, context.DeadlineExceeded
		})
		known := map[string]string{"x1.test:1": testNodeID(1), "x2.test:1": testNodeID(2), "x3.test:1": testNodeID(3)}
		known["a.test:5"] = testNodeID(5)
		confirmTestPeers(t, d.book, known)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		claims := map[string]string{"z1.test:9": testNodeID(1), "z2.test:9": testNodeID(2), "z3.test:9": testNodeID(3)}
		if response := cooldownRequest(d, Message{Address: "c.test:6", NodeID: testNodeID(6), Peers: claims}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		synctest.Wait()
		for address := range claims {
			if log.count(address) != 0 {
				t.Fatalf("verification bypassed busy exchange slots: %v", log.all())
			}
		}
		close(release)
		synctest.Wait()
		for address := range claims {
			if log.count(address) != 1 {
				t.Fatalf("requests = %v, want one probe for %s", log.all(), address)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if maxAct != exchangeConcurrency {
			t.Fatalf("maximum concurrent requests = %d, want %d", maxAct, exchangeConcurrency)
		}
	})
}

func TestDiscoveryVerificationBacklogDoesNotStarveRegularPeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log requestLog
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			if r.URL.Host == "regular.test:1" {
				return messageResponse(Message{Address: r.URL.Host, NodeID: testNodeID(1)}), nil
			}
			if r.URL.Host == "sender.test:5" {
				return messageResponse(Message{Address: r.URL.Host, NodeID: testNodeID(5)}), nil
			}
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
			return nil, context.DeadlineExceeded
		})
		confirmTestPeers(t, d.book, map[string]string{"regular.test:1": testNodeID(1), "old.test:7": testNodeID(7)})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		claims := make(map[string]string)
		for i := range MaxPeers - 1 {
			claims[fmt.Sprintf("unreachable-%02d.test:1", i)] = testNodeID(7)
		}
		if response := cooldownRequest(d, Message{Address: "sender.test:5", NodeID: testNodeID(5), Peers: claims}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		time.Sleep(37 * time.Second)
		synctest.Wait()
		if count := log.count("regular.test:1"); count != 2 {
			t.Fatalf("regular peer requests = %d, want 2 despite slow verification backlog", count)
		}
		if count := log.count(fmt.Sprintf("unreachable-%02d.test:1", MaxPeers-2)); count != 0 {
			t.Fatal("fixture failed to maintain a verification backlog")
		}
	})
}

func TestDiscoveryInboundMoveRequiresVerification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id1, id5 := testNodeID(1), testNodeID(5)
		var log requestLog
		release := make(chan struct{})
		moveStarted := make(chan struct{})
		releaseMove := make(chan struct{})
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			log.add(r.URL.Host)
			switch r.URL.Host {
			case "z.test:9":
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return messageResponse(Message{Address: "z.test:9", NodeID: id1}), nil
			case "x.test:1":
				return nil, context.DeadlineExceeded
			case "w.test:4":
				close(moveStarted)
				select {
				case <-releaseMove:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return messageResponse(Message{Address: "w.test:4", NodeID: id1}), nil
			}
			return nil, fmt.Errorf("unexpected request to %s", r.URL.Host)
		})
		original := map[string]string{"a.test:5": id5, "x.test:1": id1}
		confirmTestPeers(t, d.book, original)
		if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"z.test:9": id1}}); response.Code != http.StatusOK {
			t.Fatalf("gossip status = %d", response.Code)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		if log.count("z.test:9") != 1 {
			t.Fatalf("requests = %v, want probe in flight", log.all())
		}
		if response := cooldownRequest(d, Message{Address: "w.test:4", NodeID: id1}); response.Code != http.StatusOK {
			t.Fatalf("inbound move status = %d", response.Code)
		}
		<-moveStarted
		want := map[string]string{"a.test:5": id5, "w.test:4": id1}
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
			t.Fatalf("unverified inbound move changed membership: %v", got)
		}
		close(releaseMove)
		synctest.Wait()
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
			t.Fatalf("verified move = %v, want %v", got, want)
		}
		close(release)
		synctest.Wait()
		if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
			t.Fatalf("late probe response changed book to %v, want %v", got, want)
		}
		if log.count("z.test:9") != 1 || log.count("w.test:4") != 1 {
			t.Fatalf("requests = %v", log.all())
		}
	})
}

func TestDiscoveryVerificationStateBoundedUnderChurn(t *testing.T) {
	id1, id5 := testNodeID(1), testNodeID(5)
	t.Run("relocated schedules are pruned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log requestLog
			var mu sync.Mutex
			liveAddress := "z0.test:1"
			d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
				log.add(r.URL.Host)
				if r.URL.Host == "a.test:5" {
					return messageResponse(Message{Address: "a.test:5", NodeID: id5}), nil
				}
				// Each new claim relocates an identity whose old address is gone.
				mu.Lock()
				live := r.URL.Host == liveAddress
				mu.Unlock()
				if !live {
					return nil, context.DeadlineExceeded
				}
				return messageResponse(Message{Address: r.URL.Host, NodeID: id1}), nil
			})
			confirmTestPeers(t, d.book, map[string]string{"a.test:5": id5, "z0.test:1": id1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.Run(ctx)
			const rounds = 8
			for i := 1; i <= rounds; i++ {
				address := fmt.Sprintf("z%d.test:1", i)
				mu.Lock()
				liveAddress = address
				mu.Unlock()
				// Let the previous address's successful outbound cooldown expire.
				time.Sleep(37 * time.Second)
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{address: id1}}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				synctest.Wait()
				if log.count(address) != 1 {
					t.Fatalf("round %d requests = %v", i, log.all())
				}
			}
			want := map[string]string{"a.test:5": id5, fmt.Sprintf("z%d.test:1", rounds): id1}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, want) {
				t.Fatalf("book = %v, want %v", got, want)
			}
			// Every completed probe's cooldown has expired and scheduled
			// exchanges have run, so only retained peers keep schedules.
			time.Sleep(6 * time.Minute)
			synctest.Wait()
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.pending) != 0 || len(d.known) != len(want) {
				t.Fatalf("pending = %v, known = %v, want schedules only for %v", d.pending, d.known, want)
			}
			for address := range want {
				if d.known[address] == nil {
					t.Fatalf("retained peer %s lost its schedule", address)
				}
			}
		})
	})
	t.Run("unreachable claims fill a bounded queue", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log requestLog
			d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
				log.add(r.URL.Host)
				return nil, context.DeadlineExceeded
			})
			original := map[string]string{"a.test:5": id5, "x.test:1": id1}
			confirmTestPeers(t, d.book, original)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.Run(ctx)
			for _, prefix := range []string{"dead", "late"} {
				claims := make(map[string]string, MaxPeers)
				for i := range MaxPeers {
					claims[fmt.Sprintf("%s-%d.test:1", prefix, i)] = id1
				}
				if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: claims}); response.Code != http.StatusOK {
					t.Fatalf("gossip status = %d", response.Code)
				}
				synctest.Wait()
			}
			late := 0
			for _, host := range log.all() {
				if strings.HasPrefix(host, "late-") {
					late++
				}
			}
			if late != 0 || log.count("dead-0.test:1") != 1 || log.count(fmt.Sprintf("dead-%d.test:1", MaxPeers-1)) != 1 {
				t.Fatalf("late probes = %d, first dead = %d, last dead = %d", late, log.count("dead-0.test:1"), log.count(fmt.Sprintf("dead-%d.test:1", MaxPeers-1)))
			}
			if got := d.book.Snapshot().Peers; !reflect.DeepEqual(got, original) {
				t.Fatalf("failed probes changed book to %v", got)
			}
			d.mu.Lock()
			pending, known := len(d.pending), len(d.known)
			d.mu.Unlock()
			if pending != MaxPeers || known != MaxPeers+len(original) {
				t.Fatalf("pending = %d, known = %d, want %d and %d", pending, known, MaxPeers, MaxPeers+len(original))
			}

			// Exhaust the three-attempt budget and let the five-minute
			// cooldown expire, releasing the queue without fresh gossip.
			time.Sleep(8 * time.Minute)
			synctest.Wait()
			d.mu.Lock()
			pending, known = len(d.pending), len(d.known)
			d.mu.Unlock()
			if pending != 0 || known != len(original) {
				t.Fatalf("expired failed hints retained pending=%d known=%d", pending, known)
			}
			if response := cooldownRequest(d, Message{Address: "a.test:5", NodeID: id5, Peers: map[string]string{"late-0.test:1": id1}}); response.Code != http.StatusOK {
				t.Fatalf("later gossip status = %d", response.Code)
			}
			synctest.Wait()
			if log.count("late-0.test:1") != 1 || log.count("dead-0.test:1") != 3 {
				t.Fatalf("later claim was starved or failed hint retried without gossip: %v", log.all())
			}
		})
	})
}

func TestDiscoveryMTLSNodesConvergeAfterRelocation(t *testing.T) {
	authority, err := ca.Open(t.TempDir()+"/ca", 16)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, clientTLS, err := authority.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	start := func(dir string, server *httptest.Server) *mtlsNode {
		n := &mtlsNode{dir: dir, server: server}
		n.address = n.server.Listener.Addr().String()
		if n.book, err = OpenBook(context.Background(), dir, n.address, nil, nil); err != nil {
			t.Fatal(err)
		}
		d := NewDiscovery(n.book, NewClient(clientTLS.Clone()), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		d.interval, d.maxBackoff = 10*time.Millisecond, 40*time.Millisecond
		n.server.Config.Handler = d.Handler()
		n.server.TLS = serverTLS.Clone()
		n.server.StartTLS()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { d.Run(ctx); close(done) }()
		n.stop = sync.OnceFunc(func() {
			cancel()
			<-done
			n.server.Close()
			n.book.Close()
		})
		t.Cleanup(n.stop)
		return n
	}
	a := start(bookTestDir(t), httptest.NewUnstartedServer(nil))
	b := start(bookTestDir(t), httptest.NewUnstartedServer(nil))
	c := start(bookTestDir(t), httptest.NewUnstartedServer(nil))
	if _, err := a.book.AddSeeds([]string{b.address}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.book.AddSeeds([]string{c.address}); err != nil {
		t.Fatal(err)
	}
	ids := map[*mtlsNode]string{a: a.book.Snapshot().NodeID, b: b.book.Snapshot().NodeID, c: c.book.Snapshot().NodeID}
	converged := func(nodes ...*mtlsNode) func() bool {
		return func() bool {
			for _, n := range nodes {
				want := make(map[string]string)
				for _, other := range nodes {
					if other != n {
						want[other.address] = ids[other]
					}
				}
				if !reflect.DeepEqual(n.book.Snapshot().Peers, want) {
					return false
				}
			}
			return true
		}
	}
	eventually(t, 3*time.Second, converged(a, b, c))

	// C restarts at a new address with its persisted identity and contacts
	// only A. B must replace C's unreachable old address with the new one.
	oldAddress := c.address
	replacement := httptest.NewUnstartedServer(nil)
	t.Cleanup(replacement.Close)
	c.stop()
	moved := start(c.dir, replacement)
	ids[moved] = ids[c]
	if moved.address == oldAddress {
		t.Fatalf("relocated node reused address %s", oldAddress)
	}
	if _, err := moved.book.AddSeeds([]string{a.address}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, converged(a, b, moved))
}

type mtlsNode struct {
	dir     string
	address string
	book    *Book
	server  *httptest.Server
	stop    func()
}

// requestLog records the destination host of every outbound exchange.
type requestLog struct {
	mu    sync.Mutex
	hosts []string
}

func (l *requestLog) add(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hosts = append(l.hosts, host)
}

func (l *requestLog) count(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.hosts {
		if h == host {
			n++
		}
	}
	return n
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.hosts...)
}

func messageResponse(message Message) *http.Response {
	if message.Peers == nil {
		message.Peers = map[string]string{}
	}
	body, _ := json.Marshal(message)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}
