package peering

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

var errUnknownPeer = errors.New("unknown peer")

// ProviderHints serves the local cache key inventory to peers and keeps
// advisory hints about the keys each peer holds. A hint says a peer listed a
// key when it was last refreshed, not that it still holds the key, and it
// grants no access. Run periodically catches up from each known peer, with
// occasional full scans. Hints and continuation cursors live only in memory.
type ProviderHints struct {
	book                *Book
	client              *http.Client
	inventory           func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error)
	logger              *slog.Logger
	self                nodeID
	wake                chan struct{} // signals Run to check membership
	refreshTimeout      time.Duration // bounds each scheduled fetch
	refreshInterval     time.Duration
	fullRefreshInterval time.Duration

	mu        sync.Mutex // guards index
	index     *hintIndex
	transfers chan struct{} // bounded outbound entry streams, separate from refresh workers

	addedMu sync.Mutex      // separate so discovery never waits for index updates
	added   map[string]bool // retained IDs needing a fresh scan after discovery
}

// NewProviderHints returns hints for the local node in book. inventory serves
// live LRU-to-MRU cache pages, such as cache.Store.KeyPage. It panics if any
// duration is nonpositive. Run immediately fetches each peer known at
// construction and each identity subsequently reported by PeersAdded.
func NewProviderHints(book *Book, client *http.Client, inventory func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error), refreshTimeout, refreshInterval, fullRefreshInterval time.Duration, logger *slog.Logger) *ProviderHints {
	if refreshTimeout <= 0 || refreshInterval <= 0 || fullRefreshInterval <= 0 {
		panic("peer hint durations must be positive")
	}
	snapshot := book.Snapshot()
	// OpenBook has already validated the local node ID.
	self, _ := parseNodeID(snapshot.NodeID)
	p := &ProviderHints{
		book:                book,
		client:              client,
		inventory:           inventory,
		logger:              logger,
		self:                self,
		wake:                make(chan struct{}, 1),
		refreshTimeout:      refreshTimeout,
		refreshInterval:     refreshInterval,
		fullRefreshInterval: fullRefreshInterval,
		index:               newHintIndex(maxHintRecords),
		transfers:           make(chan struct{}, 8),
		added:               make(map[string]bool),
	}
	for _, id := range snapshot.Peers {
		p.added[id] = true
	}
	return p
}

// Handler serves the local inventory at GET /v1/cache/keys.
func (p *ProviderHints) Handler() http.Handler { return newKeysHandler(p.self, p.inventory) }

// PeerHandler routes inventory and entry requests to their cache handlers and
// every other request to discovery, which rejects unknown paths.
func PeerHandler(discovery, keys, entries http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case keysPath:
			keys.ServeHTTP(w, r)
		case entryPath:
			entries.ServeHTTP(w, r)
		default:
			discovery.ServeHTTP(w, r)
		}
	})
}

// refresh fetches the peer's inventory from after, retaining validated progress
// and applied hints on failure. Run owns concurrency and refreshTimeout bounds
// each attempt. Unknown IDs, the local node, and unknown addresses are rejected.
func (p *ProviderHints) refresh(ctx context.Context, peer string, after *cache.KeyPosition) error {
	id, ok := parseNodeID(peer)
	if !ok || id == p.self {
		return errUnknownPeer
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.refreshTimeout)
	defer cancel()
	address := addressForID(p.book.Snapshot().Peers, peer)
	if address == "" {
		return errUnknownPeer
	}
	err := fetchKeys(ctx, p.client, address, id, after, func(keys []cacheKey) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.index.merge(id, keys)
	})
	if err != nil {
		return errors.Join(ctx.Err(), err)
	}
	return nil
}

// Providers returns sorted provider IDs for key that are still in the current
// address book. Departed IDs retain their claims internally until whole-key
// eviction, so their claims become visible if they rejoin.
func (p *ProviderHints) Providers(key string) []string {
	parsed, ok := parseCacheKey(key)
	if !ok {
		return nil
	}
	p.mu.Lock()
	ids := p.index.providers(parsed)
	p.mu.Unlock()
	peers := p.book.Snapshot().Peers
	return slices.DeleteFunc(ids, func(id string) bool { return addressForID(peers, id) == "" })
}
