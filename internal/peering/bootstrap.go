package peering

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// bootstrapNodeID never advertises the candidate. One peer snapshot supplies
// best-effort avoidance for a fresh ID. Persisted IDs require a direct claim
// from another advertised address, not merely a stale third-party record.
func bootstrapNodeID(ctx context.Context, client *http.Client, self, id string, fresh bool, peers map[string]string, seeds []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	destinations := make(map[string]string, len(peers)+len(seeds))
	known := make(map[string]bool, len(peers))
	verify := make(map[string]string)
	for address, peerID := range peers {
		destinations[address] = peerID
		known[peerID] = true
	}
	for _, address := range seeds {
		canonical, err := CanonicalAddress(address)
		if err != nil {
			return "", errBook
		}
		if _, exists := destinations[canonical]; !exists {
			destinations[canonical] = ""
		}
	}
	delete(destinations, self)
	// A fast unrelated snapshot must not hide a clone among the original
	// destinations when the other initial requests are canceled.
	for address, peerID := range destinations {
		verify[address] = peerID
	}
	snapshots := probeSnapshots(ctx, client, identityFirst(destinations, id), func(Message) bool { return true })
	for _, snapshot := range snapshots {
		if !fresh && snapshot.NodeID == id && snapshot.Address != self {
			return "", fmt.Errorf("persisted peer node ID %s conflicts with %s: %w", id, snapshot.Address, errIdentityCollision)
		}
	}
	if len(snapshots) > 0 {
		snapshot := snapshots[0]
		for address, peerID := range snapshot.Peers {
			known[peerID] = true
			if peerID == id && address != self && address != snapshot.Address {
				verify[address] = peerID
			}
		}
		known[snapshot.NodeID] = true
	}
	if fresh {
		for known[id] {
			var err error
			id, err = newNodeID()
			if err != nil {
				return "", err
			}
		}
		return id, nil
	}
	conflicts := probeSnapshots(ctx, client, identityFirst(verify, id), func(message Message) bool {
		return message.NodeID == id && message.Address != self
	})
	if len(conflicts) > 0 {
		return "", fmt.Errorf("persisted peer node ID %s conflicts with %s: %w", id, conflicts[0].Address, errIdentityCollision)
	}
	return id, nil
}

// identityFirst keeps matching-ID claims ahead of unrelated addresses so
// stalled peers cannot consume every request slot before a suspect is tried.
func identityFirst(peers map[string]string, id string) []string {
	addresses := sortedAddresses(peers)
	sort.SliceStable(addresses, func(i, j int) bool {
		return peers[addresses[i]] == id && peers[addresses[j]] != id
	})
	return addresses
}

// probeSnapshots cancels on the first accepted snapshot, then collects all
// completed snapshots and joins every request. A slow address cannot block
// a healthy one in another slot, and cancellation never discards evidence.
func probeSnapshots(ctx context.Context, client *http.Client, addresses []string, accept func(Message) bool) []Message {
	ctx, cancel := context.WithCancel(ctx)
	jobs := make(chan string, len(addresses))
	for _, address := range addresses {
		jobs <- address
	}
	close(jobs)
	workers := min(exchangeConcurrency, len(addresses))
	results := make(chan Message, workers)
	var requests sync.WaitGroup
	defer func() { cancel(); requests.Wait() }()
	for range workers {
		requests.Go(func() {
			for address := range jobs {
				if ctx.Err() != nil {
					break
				}
				message, err := readSnapshot(ctx, client, address)
				if err == nil && accept(message) {
					results <- message
					return
				}
			}
			results <- Message{}
		})
	}
	var snapshots []Message
	for range workers {
		if message := <-results; message.NodeID != "" {
			snapshots = append(snapshots, message)
			cancel()
		}
	}
	return snapshots
}
