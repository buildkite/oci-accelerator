package peering

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const (
	// hintFanout is the agreed propagation policy: each forwarding pass aims
	// for this many distinct acknowledged destinations in total.
	hintFanout = 2
	// hintAttempts caps dispatches per pass, including successful ones.
	hintAttempts = 6

	availabilityConcurrency    = 4
	availabilityCadence        = time.Second
	availabilityRequestTimeout = 5 * time.Second
	availabilityMaxBackoff     = 30 * time.Second
	// availabilityScanLimit bounds the due hints examined in one round.
	availabilityScanLimit = availabilityConcurrency * maxAvailabilityClaim
)

var errAvailabilityIdentity = errors.New("invalid local node identity")

// Inventory reports the local cache's committed keys. Changes must return the
// same never-closed channel on every call, and a receive means Keys may have
// changed.
type Inventory interface {
	Keys() []string
	Changes() <-chan struct{}
}

// Availability originates hints for the local cache inventory, merges hints
// received from peers, and relays each hint with limited gossip to at most
// hintFanout acknowledged destinations. Hints are advisory. Delivery is best
// effort, with no withdrawal, expiry, or repair.
type Availability struct {
	book      *Book
	inventory Inventory
	changes   <-chan struct{}
	client    *http.Client
	wake      chan struct{}
	send      func(context.Context, string, availabilityMessage) error

	mu           sync.Mutex // guards the fields below
	self         nodeID
	index        *hintIndex
	baseline     []cacheKey // sorted cache inventory at the last reconciliation
	backoff      map[destination]*destinationBackoff
	busy         map[nodeID]bool // destinations with a request in flight
	blockedUntil time.Time       // when the last dispatch found no idle destination, the next chance of one
	random       *rand.Rand
	now          func() time.Time
	acknowledged uint64
	failed       uint64
}

// AvailabilityStats is a point-in-time view of availability state.
type AvailabilityStats struct {
	Hints        int
	Evictions    uint64
	Acknowledged uint64
	Failed       uint64
}

// NewAvailability subscribes to inventory changes, then originates hints for
// the current inventory, so no change between the two can be missed.
func NewAvailability(book *Book, inventory Inventory, client *http.Client) (*Availability, error) {
	self, ok := parseNodeID(book.Snapshot().NodeID)
	if !ok {
		return nil, errAvailabilityIdentity
	}
	a := &Availability{
		book:      book,
		inventory: inventory,
		client:    client,
		wake:      make(chan struct{}, 1),
		send: func(ctx context.Context, address string, message availabilityMessage) error {
			return sendAvailability(ctx, client, address, message)
		},
		self:    self,
		index:   newHintIndex(self),
		backoff: make(map[destination]*destinationBackoff),
		busy:    make(map[nodeID]bool),
		random:  rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		now:     time.Now,
	}
	a.changes = inventory.Changes()
	a.reconcile(inventory.Keys())
	return a, nil
}

// Handler serves availability batches addressed to the local node.
func (a *Availability) Handler() http.Handler { return newAvailabilityHandler(a.self, a.accept) }

// PeerHandler routes availability requests to availability and every other
// request to discovery, which rejects unknown paths.
func PeerHandler(discovery, availability http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == availabilityPath {
			availability.ServeHTTP(w, r)
			return
		}
		discovery.ServeHTTP(w, r)
	})
}

// Owners returns the sorted node IDs advertised as holding key. The result is
// advisory: an owner may have evicted the entry, and a hint grants no access.
func (a *Availability) Owners(key string) []string {
	parsed, ok := parseCacheKey(key)
	if !ok {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.index.owners(parsed)
}

// Stats reports retained hints and cumulative eviction and send counts.
func (a *Availability) Stats() AvailabilityStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AvailabilityStats{Hints: a.index.len(), Evictions: a.index.evictions, Acknowledged: a.acknowledged, Failed: a.failed}
}

// Run tracks inventory changes and forwards pending hints until ctx ends. It
// keeps at most availabilityConcurrency requests in flight, one per
// destination. Before returning it joins them and applies their results, so
// no request remains reserved.
func (a *Availability) Run(ctx context.Context) {
	type result struct {
		batch availabilityBatch
		err   error
	}
	// The buffer holds every possible result, so requests never block.
	results := make(chan result, availabilityConcurrency)
	var requests sync.WaitGroup
	active := 0
	defer func() {
		requests.Wait()
		for ; active > 0; active-- {
			r := <-results
			if r.err != nil {
				// Cancellation says nothing about the destination.
				a.abandon(r.batch)
			} else {
				a.complete(r.batch, nil)
			}
		}
		if a.client != nil {
			a.client.CloseIdleConnections()
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for ctx.Err() == nil {
		for _, batch := range a.dispatch(availabilityConcurrency - active) {
			active++
			requests.Add(1)
			go func() {
				defer requests.Done()
				requestCtx, cancel := context.WithTimeout(ctx, availabilityRequestTimeout)
				defer cancel()
				results <- result{batch: batch, err: a.send(requestCtx, batch.to.address, batch.message)}
			}()
		}
		timer.Reset(a.nextWait(active))
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-a.changes:
			a.reconcile(a.inventory.Keys())
		case <-timer.C:
		case result := <-results:
			active--
			a.complete(result.batch, result.err)
		}
	}
}

// accept merges a validated batch and wakes Run to schedule new hints.
func (a *Availability) accept(message availabilityMessage) error {
	a.mu.Lock()
	now := a.now()
	for _, c := range message.claims {
		a.index.merge(c, now.Add(a.jitter(availabilityCadence)))
	}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}

// nextWait returns how long Run may sleep before due work can make progress.
// With every request slot or every destination busy, only a result can.
func (a *Availability) nextWait(active int) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	due, ok := a.index.nextDue()
	if !ok || active == availabilityConcurrency {
		return time.Hour
	}
	if a.blockedUntil.After(due) {
		due = a.blockedUntil
	}
	return max(due.Sub(a.now()), 0)
}

// jitter requires mu. It spreads d by plus or minus 20 percent.
func (a *Availability) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + a.random.Float64()*0.4))
}
