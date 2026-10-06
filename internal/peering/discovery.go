package peering

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
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
	mu         sync.Mutex // guards schedules and inbound merge versus outbound scheduling
	known      map[string]*peerSchedule
}

type peerSchedule struct {
	next     time.Time
	failures int
	running  bool
}

func NewDiscovery(book *Book, client *http.Client, logger *slog.Logger) *Discovery {
	return &Discovery{
		book: book, client: client, logger: logger, wake: make(chan struct{}, 1),
		interval: 30 * time.Second, maxBackoff: 5 * time.Minute,
		known: make(map[string]*peerSchedule),
	}
}

func (d *Discovery) Handler() http.Handler { return NewHandler(d.receiveInbound) }

// Run exchanges with known peers unless a recent inbound exchange deferred them,
// then retries each independently.
// Cancellation joins all requests before returning. Unreachable peers remain
// in the address book and never prevent the proxy from serving.
func (d *Discovery) Run(ctx context.Context) {
	type result struct {
		address string
		err     error
	}
	results := make(chan result, exchangeConcurrency)
	var requests sync.WaitGroup
	defer d.client.CloseIdleConnections()
	defer requests.Wait()
	timer := time.NewTimer(0)
	defer timer.Stop()
	active := 0
	for ctx.Err() == nil {
		d.mu.Lock()
		now := time.Now()
		snapshot := d.book.Snapshot()
		wait := d.interval
		for _, address := range snapshot.Peers {
			if d.known[address] == nil {
				d.known[address] = &peerSchedule{}
			}
		}
		// Oldest due first keeps slow or failing peers from starving addresses
		// later in the book. Untried addresses have the earliest possible time.
		sort.SliceStable(snapshot.Peers, func(i, j int) bool {
			return d.known[snapshot.Peers[i]].next.Before(d.known[snapshot.Peers[j]].next)
		})
		for _, address := range snapshot.Peers {
			entry := d.known[address]
			if entry.running {
				continue
			}
			if !entry.next.After(now) && active < exchangeConcurrency {
				entry.running = true
				active++
				requests.Add(1)
				go func() {
					defer requests.Done()
					message, err := Exchange(ctx, d.client, address, snapshot)
					if err == nil && ctx.Err() == nil {
						_, err = d.receive(message)
					}
					results <- result{address, err}
				}()
			} else if remaining := entry.next.Sub(now); remaining > 0 && remaining < wait {
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
			d.mu.Lock()
			entry := d.known[result.address]
			entry.running = false
			if result.err != nil {
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
	response, err := d.receive(message)
	if err == nil && slices.Contains(response.Peers, message.Address) {
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

func (d *Discovery) receive(message Message) (Message, error) {
	rejected, err := d.book.Merge(message)
	if err != nil {
		d.logger.Error("peer address book update failed")
		return Message{}, err
	}
	if rejected != 0 {
		d.logger.Warn("peer address book full", "rejected", rejected, "limit", MaxPeers)
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return d.book.Snapshot(), nil
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
