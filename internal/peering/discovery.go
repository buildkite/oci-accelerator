package peering

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"
)

const exchangeConcurrency = 4

// Discovery exchanges a durable address book with peers. Run owns outbound
// scheduling, while Handler admits authenticated inbound exchanges.
type Discovery struct {
	book       *Book
	client     *http.Client
	logger     *slog.Logger
	wake       chan struct{}
	interval   time.Duration
	maxBackoff time.Duration
	newPeers   func(ids []string)
	mu         sync.Mutex // guards schedules and inbound merge versus outbound scheduling
	known      map[string]*peerSchedule
	pending    map[string]bool // bounded verification work, false while cooling down
}

type peerSchedule struct {
	next     time.Time
	failures int
	running  bool
}

// NewDiscovery returns discovery for book. When newPeers is not nil, it
// receives the sorted node IDs that each inbound or outbound exchange adds to
// the book, even when that exchange then fails to sync the book. An ID that
// leaves the book and returns is reported again. newPeers runs while
// discovery holds its lock, so it must not block or call back into Discovery.
func NewDiscovery(book *Book, client *http.Client, logger *slog.Logger, newPeers func(ids []string)) *Discovery {
	return &Discovery{
		book: book, client: client, logger: logger, newPeers: newPeers, wake: make(chan struct{}, 1),
		interval: 30 * time.Second, maxBackoff: 5 * time.Minute,
		known:   make(map[string]*peerSchedule),
		pending: make(map[string]bool),
	}
}

func (d *Discovery) Handler() http.Handler { return NewHandler(d.receiveInbound) }

// Run exchanges with known peers unless a recent inbound exchange deferred them,
// then retries each independently.
// Cancellation joins all requests before returning. Unreachable peers remain
// in the address book and never prevent the proxy from serving.
func (d *Discovery) Run(ctx context.Context) {
	type result struct {
		address      string
		message      Message
		before       bookView
		err          error
		verification bool
	}
	results := make(chan result, exchangeConcurrency)
	var requests sync.WaitGroup
	defer d.client.CloseIdleConnections()
	defer requests.Wait()
	timer := time.NewTimer(0)
	defer timer.Stop()
	active := 0
	verifying := 0
	for ctx.Err() == nil {
		d.mu.Lock()
		now := time.Now()
		snapshot, before := d.book.observe()
		wait := d.interval
		addresses := d.destinations(before, now)
		// Oldest due first keeps slow or failing peers from starving addresses
		// later in the book. Untried addresses have the earliest possible time.
		sort.SliceStable(addresses, func(i, j int) bool {
			return d.known[addresses[i]].next.Before(d.known[addresses[j]].next)
		})
		for _, address := range addresses {
			entry := d.known[address]
			if entry.running {
				continue
			}
			_, retained := before.peers[address]
			verification := !retained
			// Unverified hints cannot monopolize all slots and starve regular
			// discovery, even when their deadlines precede every known peer.
			if verification && verifying >= exchangeConcurrency/2 {
				continue
			}
			if !entry.next.After(now) && active < exchangeConcurrency {
				entry.running = true
				active++
				if verification {
					verifying++
				}
				requests.Add(1)
				go func() {
					defer requests.Done()
					message, err := Exchange(ctx, d.client, address, snapshot)
					results <- result{address: address, message: message, before: before, err: err, verification: verification}
				}()
			} else if remaining := entry.next.Sub(now); remaining > 0 && remaining < wait {
				wait = remaining
			}
		}
		for address, pending := range d.pending {
			if remaining := d.known[address].next.Sub(now); !pending && remaining > 0 && remaining < wait {
				wait = remaining
			}
		}
		d.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-timer.C:
		case result := <-results:
			active--
			if result.verification {
				verifying--
			}
			d.mu.Lock()
			entry := d.known[result.address]
			entry.running = false
			if result.err == nil && ctx.Err() == nil {
				_, result.err = d.receive(result.message, result.address, &result.before)
			}
			if result.err != nil {
				// Failed hints retain backoff but release their queue slots after
				// it expires. Only fresh gossip requests another verification.
				if _, pending := d.pending[result.address]; pending {
					d.pending[result.address] = false
				}
			}
			if result.err != nil && !errors.Is(result.err, errStaleObservation) {
				if entry.failures == 0 {
					d.logger.Warn("peer exchange unavailable", "peer", result.address)
				}
				if entry.failures < 10 {
					entry.failures++
				}
			} else {
				if entry.failures != 0 {
					d.logger.Info("peer exchange recovered", "peer", result.address)
				}
				entry.failures = 0
			}
			d.deferExchange(entry, entry.failures)
			d.mu.Unlock()
		}
	}
}

func (d *Discovery) receiveInbound(message Message) (Message, error) {
	// Do not let Run schedule a newly merged sender before its cooldown is set.
	d.mu.Lock()
	defer d.mu.Unlock()
	response, err := d.receive(message, "", nil)
	if err == nil && response.Peers[message.Address] == message.NodeID {
		entry := d.known[message.Address]
		if entry == nil {
			entry = &peerSchedule{}
			d.known[message.Address] = entry
		}
		// Inbound contact is not evidence that the advertised listener is reachable.
		// Preserve failure backoff and only track senders retained in the book.
		d.deferExchange(entry, 0)
	}
	return response, err
}

// receive requires mu. Network verification is queued, never performed while
// holding the scheduler or book locks or while serving an inbound exchange.
func (d *Discovery) receive(message Message, destination string, before *bookView) (Message, error) {
	result, err := d.book.merge(message, destination, before)
	// A merge can publish new identities and still report a sync failure.
	if len(result.Added) != 0 && d.newPeers != nil {
		d.newPeers(result.Added)
	}
	if err != nil {
		if errors.Is(err, errIdentityCollision) {
			d.logger.Warn("peer identity collision", "peer", message.Address)
		} else if !errors.Is(err, errStaleObservation) {
			d.logger.Error("peer address book update failed")
		}
		return Message{}, err
	}
	response, view := d.book.observe()
	d.prune(view, time.Now())
	for _, address := range []string{destination, message.Address} {
		if _, exists := d.pending[address]; exists {
			d.pending[address] = false
		}
	}
	if result.Rejected != 0 {
		d.logger.Warn("peer address book full", "rejected", result.Rejected, "limit", MaxPeers)
	}
	dropped := 0
	for _, address := range result.Verify {
		if _, exists := d.pending[address]; !exists && len(d.pending) == MaxPeers {
			dropped++
			continue
		}
		d.pending[address] = true
		if d.known[address] == nil {
			d.known[address] = &peerSchedule{}
		}
	}
	if dropped != 0 {
		d.logger.Warn("peer verification queue full", "rejected", dropped, "limit", MaxPeers)
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return response, nil
}

// destinations requires mu. Pending verifications share scheduling and backoff
// with regular discovery. Completed probes retain a cooldown until expiry so
// repeated stale gossip cannot create a tight loop. Removed peers are pruned
// only after their in-flight requests have returned.
func (d *Discovery) destinations(view bookView, now time.Time) []string {
	d.prune(view, now)
	addresses := sortedAddresses(view.peers)
	for address, pending := range d.pending {
		if _, retained := view.peers[address]; pending && !retained {
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		if d.known[address] == nil {
			d.known[address] = &peerSchedule{}
		}
	}
	return addresses
}

func (d *Discovery) prune(view bookView, now time.Time) {
	for address, pending := range d.pending {
		entry := d.known[address]
		if !pending && !entry.running && !entry.next.After(now) {
			delete(d.pending, address)
		}
	}
	for address, entry := range d.known {
		_, retained := view.peers[address]
		_, verifying := d.pending[address]
		if !retained && !verifying && !entry.running {
			delete(d.known, address)
		}
	}
}

// deferExchange requires mu. Neither direction may shorten an existing delay.
func (d *Discovery) deferExchange(entry *peerSchedule, failures int) {
	delay := retryDelay(d.interval, d.maxBackoff, failures)
	// +/-20% jitter avoids synchronized full-list exchanges after startup.
	delay = time.Duration(float64(delay) * (0.8 + rand.Float64()*0.4))
	if next := time.Now().Add(delay); next.After(entry.next) {
		entry.next = next
	}
}

func retryDelay(interval, maximum time.Duration, failures int) time.Duration {
	delay := interval
	for i := 1; i < failures && delay < maximum; i++ {
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
