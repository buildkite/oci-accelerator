package peering

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	announceConcurrency    = 4
	announceCadence        = time.Second
	announceRequestTimeout = 5 * time.Second
	announceMaxBackoff     = 30 * time.Second
	// announceScanLimit bounds the due hints examined in one round.
	announceScanLimit = announceConcurrency * maxAnnouncementHints
)

var errHintIdentity = errors.New("invalid local node identity")

// Inventory reports the local cache's committed keys. Changes must return the
// same never-closed channel on every call, and a receive means Keys may have
// changed.
type Inventory interface {
	Keys() []string
	Changes() <-chan struct{}
}

// ProviderHints announces hints for the local cache inventory, merges hints
// received from peers, and relays each hint with limited gossip to at most
// hintFanout acknowledged destinations. It checkpoints retained hints into the
// book's state directory under the book's lock. Hints are advisory. Delivery
// is best effort, with no withdrawal, expiry, or repair.
type ProviderHints struct {
	book            *Book
	inventory       Inventory
	changes         <-chan struct{}
	client          *http.Client
	logger          *slog.Logger
	wake            chan struct{}
	send            func(context.Context, string, announcement) error
	persist         func(write func(io.Writer) error) error
	checkpointEvery time.Duration
	now             func() time.Time // fixed before Run starts

	mu           sync.Mutex // guards the fields below
	self         nodeID
	index        *hintIndex
	baseline     []cacheKey // sorted cache inventory at the last reconciliation
	backoff      map[destination]*destinationBackoff
	busy         map[nodeID]bool // destinations with a request in flight
	blockedUntil time.Time       // when the last dispatch found no idle destination, the next chance of one
	random       *rand.Rand
	acknowledged uint64
	failed       uint64
	// revision advances with every change to persisted state, and saved is
	// the revision of the last successful checkpoint.
	revision, saved    uint64
	checkpointFailures uint64
	checkpointFailing  bool
}

// ProviderHintStats is a point-in-time view of provider hint state.
type ProviderHintStats struct {
	Hints              int
	Evictions          uint64
	Acknowledged       uint64
	Failed             uint64
	CheckpointFailures uint64
}

// NewProviderHints restores the checkpointed hints in book's state directory,
// subscribes to inventory changes, then reconciles the restored hints with
// the current inventory, so no change between the two can be missed. A
// missing, corrupt, oversized, unsupported, or foreign snapshot starts with
// no hints. Unsafe or unreadable state fails, like other peer state.
func NewProviderHints(book *Book, inventory Inventory, client *http.Client, logger *slog.Logger) (*ProviderHints, error) {
	self, ok := parseNodeID(book.Snapshot().NodeID)
	if !ok {
		return nil, errHintIdentity
	}
	a := &ProviderHints{
		book:      book,
		inventory: inventory,
		client:    client,
		logger:    logger,
		wake:      make(chan struct{}, 1),
		send: func(ctx context.Context, address string, message announcement) error {
			return sendAnnouncement(ctx, client, address, message)
		},
		persist: func(write func(io.Writer) error) error {
			return persistHintSnapshot(book.dir, write)
		},
		checkpointEvery: hintCheckpointInterval,
		now:             time.Now,
		self:            self,
		index:           newHintIndex(self),
		backoff:         make(map[destination]*destinationBackoff),
		busy:            make(map[nodeID]bool),
		random:          rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
	hints, err := loadHintSnapshot(book.dir, self)
	switch {
	case errors.Is(err, errStateCorrupt), errors.Is(err, errStateOversized), errors.Is(err, errStateVersion), errors.Is(err, errStateIdentity):
		logger.Warn("provider hint state discarded", "reason", err.Error())
		a.revision++ // replace the discarded snapshot at the next checkpoint
	case err != nil:
		return nil, err
	}
	a.restore(hints)
	a.changes = inventory.Changes()
	a.restoreInventory(inventory.Keys())
	return a, nil
}

// Handler serves announcements addressed to the local node.
func (a *ProviderHints) Handler() http.Handler { return newAnnounceHandler(a.self, a.accept) }

// PeerHandler routes announcements to announce and every other request to
// discovery, which rejects unknown paths.
func PeerHandler(discovery, announce http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == announcePath {
			announce.ServeHTTP(w, r)
			return
		}
		discovery.ServeHTTP(w, r)
	})
}

// Providers returns the sorted node IDs advertised as holding key. The result is
// advisory: a provider may have evicted the entry, and a hint grants no access.
func (a *ProviderHints) Providers(key string) []string {
	parsed, ok := parseCacheKey(key)
	if !ok {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.index.providers(parsed)
}

// Stats reports retained hints and cumulative eviction, send, and checkpoint
// failure counts.
func (a *ProviderHints) Stats() ProviderHintStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ProviderHintStats{
		Hints: a.index.len(), Evictions: a.index.evictions, Acknowledged: a.acknowledged,
		Failed: a.failed, CheckpointFailures: a.checkpointFailures,
	}
}

// Run tracks inventory changes, forwards pending hints, and checkpoints
// changed state until ctx ends. It keeps at most announceConcurrency
// requests in flight, one per destination, and at most one checkpoint write.
// Before returning it joins both and applies request results, so no request
// remains reserved. Call Checkpoint afterwards to save the final state.
func (a *ProviderHints) Run(ctx context.Context) {
	type result struct {
		batch announcementBatch
		err   error
	}
	// The buffers hold every possible result, so writers never block.
	results := make(chan result, announceConcurrency)
	checkpoints := make(chan checkpointResult, 1)
	var requests sync.WaitGroup
	active := 0
	writing := false
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
		if writing {
			r := <-checkpoints
			_ = a.finishCheckpoint(r.revision, r.err)
		}
		if a.client != nil {
			a.client.CloseIdleConnections()
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	// A fixed cadence, rather than a quiet period, keeps continuous changes
	// from postponing checkpoints.
	nextCheckpoint := a.now().Add(a.checkpointEvery)
	for ctx.Err() == nil {
		for _, batch := range a.dispatch(announceConcurrency - active) {
			active++
			requests.Add(1)
			go func() {
				defer requests.Done()
				requestCtx, cancel := context.WithTimeout(ctx, announceRequestTimeout)
				defer cancel()
				results <- result{batch: batch, err: a.send(requestCtx, batch.to.address, batch.message)}
			}()
		}
		wait := a.nextWait(active)
		if !writing && a.dirty() {
			if now := a.now(); now.Before(nextCheckpoint) {
				wait = min(wait, nextCheckpoint.Sub(now))
			} else {
				snapshot := a.capture()
				writing = true
				nextCheckpoint = now.Add(a.checkpointEvery)
				go func() { checkpoints <- checkpointResult{revision: snapshot.revision, err: a.writeCheckpoint(snapshot)} }()
			}
		}
		timer.Reset(wait)
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
		case r := <-checkpoints:
			writing = false
			_ = a.finishCheckpoint(r.revision, r.err)
		}
	}
}

// accept merges a validated batch and wakes Run to schedule new hints.
func (a *ProviderHints) accept(message announcement) error {
	a.mu.Lock()
	now := a.now()
	for _, c := range message.hints {
		if a.index.merge(c, now.Add(a.jitter(announceCadence))) {
			a.revision++
		}
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
func (a *ProviderHints) nextWait(active int) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	due, ok := a.index.nextDue()
	if !ok || active == announceConcurrency {
		return time.Hour
	}
	if a.blockedUntil.After(due) {
		due = a.blockedUntil
	}
	return max(due.Sub(a.now()), 0)
}

// restore admits checkpointed hints in FIFO order. Pending passes resume with
// fresh jitter and their remaining budgets.
func (a *ProviderHints) restore(hints []*hint) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for _, h := range hints {
		a.index.add(h)
		if h.pending {
			a.index.schedule(h, now.Add(a.jitter(announceCadence)))
		}
	}
	// Smaller limits than the snapshot was written under evict its oldest
	// records, and the next checkpoint records that.
	if a.index.evictions != 0 {
		a.revision++
	}
}

// jitter requires mu. It spreads d by plus or minus 20 percent.
func (a *ProviderHints) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + a.random.Float64()*0.4))
}
