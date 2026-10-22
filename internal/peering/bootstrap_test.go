package peering

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenBookAvoidsFreshIDCollisionsBeforePersistence(t *testing.T) {
	// Force two collisions, first with the responder and then with its gossip.
	// No TLS or other background randomness runs in this test.
	previous := rand.Reader
	rand.Reader = bytes.NewReader(append(append(make([]byte, 9), bytes.Repeat([]byte{1}, 9)...), bytes.Repeat([]byte{2}, 9)...))
	t.Cleanup(func() { rand.Reader = previous })
	dir := bookTestDir(t)
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/peers" || r.Body != nil {
			t.Errorf("bootstrap advertised identity: %s %s, body %v", r.Method, r.URL.Path, r.Body)
		}
		if _, err := os.Stat(filepath.Join(dir, nodeIDName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("identity persisted before check: %v", err)
		}
		return bootstrapResponse(Message{Address: "seed.test:1", NodeID: "AAAAAAAAAAAA", Peers: map[string]string{"other.test:2": "AQEBAQEBAQEB"}}), nil
	})}
	book, err := OpenBook(context.Background(), dir, "self.test:1", []string{"seed.test:1"}, client)
	if err != nil {
		t.Fatal(err)
	}
	if got := book.Snapshot().NodeID; got != "AgICAgICAgIC" {
		t.Fatalf("fresh ID = %q, want third random candidate", got)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("bootstrap requests = %d, want 1", calls.Load())
	}
	book, err = OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if got := book.Snapshot().NodeID; got != "AgICAgICAgIC" {
		t.Fatalf("reopened ID = %q", got)
	}
}

func TestOpenBookVerifiesPersistedIDCollisions(t *testing.T) {
	for _, test := range []struct {
		name        string
		address     string
		id          string
		probeErr    error
		collision   bool
		localClaim  bool
		seededClone bool
	}{
		{name: "clone discovered through gossip", address: "other.test:2", id: testNodeID(1), collision: true},
		{name: "clone retained locally", address: "other.test:2", id: testNodeID(1), collision: true, localClaim: true},
		{name: "fast unrelated seed cannot hide clone", address: "other.test:2", id: testNodeID(1), collision: true, seededClone: true},
		{name: "reused address", address: "other.test:2", id: testNodeID(3)},
		{name: "alias reaches self", address: "self.test:1", id: testNodeID(1)},
		{name: "unreachable old address", probeErr: errExchange},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, nodeIDName), []byte(testNodeID(1)), 0o600); err != nil {
				t.Fatal(err)
			}
			peers := map[string]bookRecord{}
			if test.localClaim {
				// Check before OpenBook removes records matching the local ID.
				peers["other.test:2"] = bookRecord{NodeID: testNodeID(1), LastSuccess: time.Now()}
			}
			if _, err := writeBookState(dir, peers); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(dir, bookStateName))
			var verified atomic.Int32
			cloneStarted := make(chan struct{})
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "other.test:2" {
					if verified.Add(1) == 1 && test.seededClone {
						close(cloneStarted)
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if test.probeErr != nil {
						return nil, test.probeErr
					}
					return bootstrapResponse(Message{Address: test.address, NodeID: test.id, Peers: map[string]string{}}), nil
				}
				gossip := map[string]string{"other.test:2": testNodeID(1)}
				if test.seededClone {
					<-cloneStarted
					gossip = map[string]string{}
				}
				return bootstrapResponse(Message{Address: "seed.test:3", NodeID: testNodeID(2), Peers: gossip}), nil
			})}
			seeds := []string{"seed.test:3"}
			if test.seededClone {
				seeds = append(seeds, "other.test:2")
			}
			book, err := OpenBook(context.Background(), dir, "self.test:1", seeds, client)
			if test.collision {
				if !errors.Is(err, errIdentityCollision) || book != nil {
					t.Fatalf("clone startup = %v, %v", book, err)
				}
				after, _ := os.ReadFile(filepath.Join(dir, bookStateName))
				if !bytes.Equal(before, after) {
					t.Fatal("failed collision check modified address book")
				}
				// Failure must release the exclusive state lock.
				book, err = OpenBook(context.Background(), dir, "self.test:1", nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			id, err := os.ReadFile(filepath.Join(dir, nodeIDName))
			if err != nil || string(id) != testNodeID(1) || book.Snapshot().NodeID != testNodeID(1) {
				t.Fatalf("persisted identity changed: %q, %v", id, err)
			}
			if verified.Load() == 0 {
				t.Fatal("did not verify gossip claim")
			}
		})
	}
}

func TestOpenBookPrioritizesKnownCollisionAddresses(t *testing.T) {
	for _, localClaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "gossip", true: "persisted"}[localClaim], func(t *testing.T) {
			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, nodeIDName), []byte(testNodeID(1)), 0o600); err != nil {
				t.Fatal(err)
			}
			if localClaim {
				// Expired membership is still useful as a collision probe target.
				peers := map[string]bookRecord{"z-clone.test:1": {NodeID: testNodeID(1), LastSuccess: time.Now().Add(-membershipTTL)}}
				if _, err := writeBookState(dir, peers); err != nil {
					t.Fatal(err)
				}
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host {
				case "a-seed.test:1":
					return bootstrapResponse(Message{Address: "a-seed.test:1", NodeID: testNodeID(2), Peers: map[string]string{"z-clone.test:1": testNodeID(1)}}), nil
				case "z-clone.test:1":
					return bootstrapResponse(Message{Address: "z-clone.test:1", NodeID: testNodeID(1), Peers: map[string]string{}}), nil
				default:
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
			})}
			seeds := []string{"a-seed.test:1", "b.test:1", "c.test:1", "d.test:1", "e.test:1"}
			if localClaim {
				seeds = seeds[1:]
			}
			book, err := OpenBook(context.Background(), dir, "self.test:1", seeds, client)
			if book != nil {
				book.Close()
			}
			if !errors.Is(err, errIdentityCollision) {
				t.Fatalf("stalled unrelated peers hid known clone: %v", err)
			}
		})
	}
}

func TestOpenBookBootstrapBudgetAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "shutdown"}[cancelParent], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var active atomic.Int32
			var calls atomic.Int32
			var deadline time.Time
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				active.Add(1)
				defer active.Add(-1)
				if calls.Add(1) == 1 {
					deadline, _ = r.Context().Deadline()
					if cancelParent {
						cancel()
					}
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			started := time.Now()
			book, err := OpenBook(ctx, bookTestDir(t), "self.test:1", []string{"a.test:1", "b.test:1", "c.test:1", "d.test:1", "e.test:1"}, client)
			if cancelParent {
				if !errors.Is(err, context.Canceled) || book != nil {
					t.Fatalf("canceled startup = %v, %v", book, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer book.Close()
				if elapsed := time.Since(started); elapsed > 6*time.Second {
					t.Fatalf("bootstrap exceeded overall budget: %v", elapsed)
				}
				if budget := deadline.Sub(started); budget < 4*time.Second || budget > 6*time.Second {
					t.Fatalf("request deadline = %v from startup", budget)
				}
			}
			if active.Load() != 0 || calls.Load() > 4 {
				t.Fatalf("requests not bounded and joined: active=%d, total=%d", active.Load(), calls.Load())
			}
		})
	}
}

func bootstrapResponse(message Message) *http.Response {
	raw, _ := json.Marshal(message)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}
}
