package peering

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

var errUnknownPeer = errors.New("unknown peer")

// ProviderHints serves the local cache key inventory to peers and keeps
// advisory hints about the keys each peer holds. A hint says a peer listed a
// key when it was last refreshed, not that it still holds the key, and it
// grants no access. Hints live only in memory, so Run refreshes every peer
// known at construction and then each peer queued by QueueRefresh.
type ProviderHints struct {
	book           *Book
	client         *http.Client
	inventory      func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error)
	logger         *slog.Logger
	self           nodeID
	refreshing     chan struct{} // holds a token while a Refresh runs
	wake           chan struct{} // signals Run that the queue has work
	refreshTimeout time.Duration // bounds a fetch after it acquires the refresh slot

	mu    sync.Mutex // guards index
	index *hintIndex

	// queueMu is separate from mu so queueing never waits for index updates.
	queueMu sync.Mutex
	queue   []string        // node IDs awaiting a refresh, oldest first
	queued  map[string]bool // the IDs in queue
}

// NewProviderHints returns hints for the local node in book, with a refresh
// queued for every peer the book already identifies. inventory serves live
// LRU-to-MRU cache pages, such as cache.Store.KeyPage.
// refreshTimeout must be positive.
func NewProviderHints(book *Book, client *http.Client, inventory func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error), refreshTimeout time.Duration, logger *slog.Logger) *ProviderHints {
	snapshot := book.Snapshot()
	// OpenBook has already validated the local node ID.
	self, _ := parseNodeID(snapshot.NodeID)
	p := &ProviderHints{
		book:           book,
		client:         client,
		inventory:      inventory,
		logger:         logger,
		self:           self,
		refreshing:     make(chan struct{}, 1),
		wake:           make(chan struct{}, 1),
		refreshTimeout: refreshTimeout,
		index:          newHintIndex(maxHintRecords),
		queued:         make(map[string]bool),
	}
	ids := make([]string, 0, len(snapshot.Peers))
	for _, id := range snapshot.Peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	p.QueueRefresh(ids)
	return p
}

// Handler serves the local inventory at GET /v1/cache/keys.
func (p *ProviderHints) Handler() http.Handler { return newKeysHandler(p.self, p.inventory) }

// PeerHandler routes cache key requests to keys and every other request to
// discovery, which rejects unknown paths.
func PeerHandler(discovery, keys http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == keysPath {
			keys.ServeHTTP(w, r)
			return
		}
		discovery.ServeHTTP(w, r)
	})
}

// Refresh fetches the cache keys of the peer with node ID peer, at its
// current address in the book, and adds that peer to each key's providers.
// Each validated page applies immediately, surviving later failure. Invalid
// IDs, the local node, and peers without a known address are rejected.
// Refreshes run one at a time. Waiting ends with ctx, and refreshTimeout
// bounds the fetch only after acquiring the slot. All peers share one
// distinct-key bound, so a large inventory can displace other peers' hints.
func (p *ProviderHints) Refresh(ctx context.Context, peer string) error {
	id, ok := parseNodeID(peer)
	if !ok || id == p.self {
		return errUnknownPeer
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.refreshing <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.refreshing }()
	ctx, cancel := context.WithTimeout(ctx, p.refreshTimeout)
	defer cancel()
	address := addressForID(p.book.Snapshot().Peers, peer)
	if address == "" {
		return errUnknownPeer
	}
	err := fetchKeys(ctx, p.client, address, id, func(keys []cacheKey) {
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
