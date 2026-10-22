package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestDiscoveryInboundDefersOnlySender(t *testing.T) {
	for _, alreadyKnown := range []bool{false, true} {
		name := "new sender"
		if alreadyKnown {
			name = "known sender"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := make(map[string]int)
				var mu sync.Mutex
				count := func(address string) int {
					mu.Lock()
					defer mu.Unlock()
					return calls[address]
				}
				d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					calls[r.URL.Host]++
					mu.Unlock()
					return nil, context.DeadlineExceeded
				})
				if alreadyKnown {
					if _, err := d.book.AddSeeds([]string{"a.test:1"}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go d.Run(ctx)
				synctest.Wait()
				before := count("a.test:1")
				if alreadyKnown && before != 1 {
					t.Fatalf("initial requests = %d, want 1", before)
				}
				time.Sleep(20 * time.Second)
				response := cooldownRequest(d, Message{Address: "A.TEST.:1", NodeID: testNodeID(1), Peers: map[string]string{"c.test:3": testNodeID(3)}})
				if response.Code != http.StatusOK {
					t.Fatalf("inbound status = %d", response.Code)
				}
				var got Message
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				want := Message{Address: "b.test:2", NodeID: d.book.Snapshot().NodeID, Peers: map[string]string{"a.test:1": testNodeID(1), "c.test:3": testNodeID(3)}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("response = %+v, want %+v", got, want)
				}
				synctest.Wait()
				if count("a.test:1") != before || count("c.test:3") != 1 {
					t.Fatalf("requests after inbound: sender=%d third-party=%d", count("a.test:1"), count("c.test:3"))
				}
				// Refresh before the first cooldown expires. At t=63 even its
				// longest jitter has expired, but the refreshed minimum has not.
				time.Sleep(20 * time.Second)
				if response := cooldownRequest(d, Message{Address: "a.test:1", NodeID: testNodeID(1)}); response.Code != http.StatusOK {
					t.Fatalf("refresh status = %d", response.Code)
				}
				time.Sleep(23 * time.Second)
				synctest.Wait()
				if count("a.test:1") != before {
					t.Fatalf("sender contacted during cooldown: %d", count("a.test:1"))
				}
				time.Sleep(14 * time.Second)
				synctest.Wait()
				if count("a.test:1") != before+1 {
					t.Fatalf("sender not contacted once after expiry: %d", count("a.test:1"))
				}
			})
		})
	}
}

func TestDiscoveryRejectedInboundDoesNotDeferSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, context.DeadlineExceeded
		})
		if _, err := d.book.AddSeeds([]string{"a.test:1"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		time.Sleep(20 * time.Second)
		invalid := Message{Address: "a.test:1", NodeID: testNodeID(1), Peers: map[string]string{"invalid": testNodeID(3)}}
		if response := cooldownRequest(d, invalid); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid exchange status = %d", response.Code)
		}
		d.book.write = func(string, map[string]string) (bool, error) { return false, syscall.ENOSPC }
		if response := cooldownRequest(d, Message{Address: "a.test:1", NodeID: testNodeID(1), Peers: map[string]string{"c.test:3": testNodeID(3)}}); response.Code != http.StatusInternalServerError {
			t.Fatalf("failed merge status = %d", response.Code)
		}
		time.Sleep(17 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("requests = %d, want retry despite rejected inbound", calls.Load())
		}
	})
}

func TestDiscoveryInboundPreservesFailureBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, context.DeadlineExceeded
		})
		if _, err := d.book.AddSeeds([]string{"a.test:1"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		// Three failures occur by t=108 at the latest. The fourth attempt
		// cannot happen before t=168 with exponential backoff and jitter.
		time.Sleep(109 * time.Second)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatalf("requests = %d, want 3", calls.Load())
		}
		if response := cooldownRequest(d, Message{Address: "a.test:1", NodeID: testNodeID(1)}); response.Code != http.StatusOK {
			t.Fatalf("inbound status = %d", response.Code)
		}
		time.Sleep(37 * time.Second)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatalf("inbound shortened failure backoff: requests = %d", calls.Load())
		}
	})
}

func TestDiscoveryInboundDuringOutboundExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		d := cooldownDiscovery(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			body := `{"address":"a.test:1","nodeId":"000000000001","peers":{}}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
		})
		if _, err := d.book.AddSeeds([]string{"a.test:1"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.Run(ctx)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial requests = %d", calls.Load())
		}
		if response := cooldownRequest(d, Message{Address: "a.test:1", NodeID: testNodeID(1)}); response.Code != http.StatusOK {
			t.Fatalf("inbound status = %d", response.Code)
		}
		synctest.Wait()
		d.mu.Lock()
		inboundNext := d.known["a.test:1"].next
		d.mu.Unlock()
		close(release)
		synctest.Wait()
		d.mu.Lock()
		next := d.known["a.test:1"].next
		d.mu.Unlock()
		if next.Before(inboundNext) {
			t.Fatal("outbound completion shortened inbound cooldown")
		}
		time.Sleep(23 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("duplicate request during cooldown: %d", calls.Load())
		}
		time.Sleep(14 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("requests after expiry = %d, want 2", calls.Load())
		}
	})
}

func TestDiscoveryCooldownStateOnlyTracksRetainedPeers(t *testing.T) {
	d := cooldownDiscovery(t, nil)
	peers := make(map[string]string, MaxPeers-1)
	for i := 1; i < MaxPeers; i++ {
		peers[fmt.Sprintf("peer-%d.test:1", i)] = testNodeID(i)
	}
	if result, err := d.book.merge(Message{Address: "peer-0.test:1", NodeID: testNodeID(0), Peers: peers}, "", nil); err != nil || result.Rejected != 0 {
		t.Fatalf("filling Merge = %+v, %v", result, err)
	}
	for _, sender := range []Message{
		{Address: "b.test:2", NodeID: d.book.Snapshot().NodeID},
		{Address: "overflow.test:1", NodeID: testNodeID(MaxPeers)},
	} {
		if response := cooldownRequest(d, sender); response.Code != http.StatusOK {
			t.Fatalf("inbound status = %d", response.Code)
		}
	}
	if len(d.known) != 0 {
		t.Fatalf("tracked self or rejected sender: %v", d.known)
	}
}

func TestDiscoveryCooldownNeverShortensExistingDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := cooldownDiscovery(t, nil)
		// The new jittered interval is always shorter than this existing delay.
		deadline := time.Now().Add(time.Minute)
		entry := &peerSchedule{next: deadline}
		d.mu.Lock()
		d.deferExchange(entry, 0)
		d.mu.Unlock()
		if !entry.next.Equal(deadline) {
			t.Fatalf("deadline = %v, want %v", entry.next, deadline)
		}
	})
}
