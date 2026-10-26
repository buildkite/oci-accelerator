package peering

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const hintRefreshConcurrency = 4

// hintSchedule is owned by Run. Workers receive copies of the cursor, and
// return their progress before Run schedules another fetch for that peer.
type hintSchedule struct {
	next, fullAt time.Time
	after        cache.KeyPosition
	stalled      int
	scanning     bool
	running      bool
	reset        bool
}

// Run catches up from known peers, oldest due first, with at most four active
// fetches and one per peer. Full scans replace incremental polls and resume
// after interruptions. Cancellation joins all fetches before returning.
func (p *ProviderHints) Run(ctx context.Context) {
	type result struct {
		id    string
		after cache.KeyPosition
		err   error
	}

	known := make(map[string]*hintSchedule)
	results := make(chan result, hintRefreshConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()

	timer := time.NewTimer(0)
	defer timer.Stop()

	active := 0
	for ctx.Err() == nil {
		now := time.Now()
		p.addedMu.Lock()
		for id := range p.added {
			if entry := known[id]; entry != nil {
				entry.reset = true
				entry.next = time.Time{}
			} else {
				known[id] = &hintSchedule{}
			}
		}
		clear(p.added)
		p.addedMu.Unlock()

		peers := p.book.Snapshot().Peers
		ids := make([]string, 0, len(peers))
		retained := make(map[string]bool, len(peers))
		for _, id := range peers {
			retained[id] = true
			// Wait for discovery's addition notification before scheduling
			// new IDs, rather than racing it into a duplicate initial scan.
			if known[id] != nil {
				ids = append(ids, id)
			}
		}
		for id, entry := range known {
			if !retained[id] && !entry.running {
				delete(known, id)
			}
		}

		sort.Slice(ids, func(i, j int) bool {
			a, b := known[ids[i]].next, known[ids[j]].next
			if a.Equal(b) {
				return ids[i] < ids[j]
			}
			return a.Before(b)
		})

		wait := p.refreshInterval
		for _, id := range ids {
			entry := known[id]
			if entry.running {
				continue
			}
			if entry.next.After(now) {
				wait = min(wait, entry.next.Sub(now))
				continue
			}
			if active == hintRefreshConcurrency {
				continue
			}
			if entry.reset || !entry.scanning && !entry.fullAt.After(now) {
				entry.after = cache.KeyPosition{}
				entry.stalled = 0
				entry.scanning = true
				entry.reset = false
			}

			entry.running = true
			active++
			workers.Add(1)
			go func(id string, after cache.KeyPosition) {
				defer workers.Done()
				err := p.refresh(ctx, id, &after)
				results <- result{id: id, after: after, err: err}
			}(id, entry.after)
		}

		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		case result := <-results:
			active--
			entry := known[result.id]
			entry.running = false
			if entry.reset {
				// A rejoining identity needs a full scan, but only after its
				// earlier fetch has released the peer's active slot.
				continue
			}

			if result.err == nil || result.after != entry.after {
				entry.stalled = 0
			} else {
				entry.stalled++
			}
			entry.after = result.after
			rejected := errors.Is(result.err, cache.ErrKeyCursor)
			if rejected || entry.stalled >= 3 {
				// Rejected or repeatedly stalled checkpoints need a fresh scan.
				// Keep the normal poll delay, never a tight reset/retry loop.
				entry.after = cache.KeyPosition{}
				entry.stalled = 0
				entry.scanning = true
			} else if result.err == nil && entry.scanning {
				entry.scanning = false
				entry.fullAt = time.Now().Add(hintJitter(p.fullRefreshInterval))
			}
			entry.next = time.Now().Add(hintJitter(p.refreshInterval))

			if result.err != nil && ctx.Err() == nil && !errors.Is(result.err, errUnknownPeer) {
				if rejected {
					p.logger.Debug("peer cache keys checkpoint rejected", "node_id", result.id)
				} else {
					p.logger.Warn("peer cache keys refresh failed", "node_id", result.id)
				}
			}
		}
	}
}

// PeersAdded requests fresh scans for newly added identities, including peers
// that depart and return between scheduler snapshots. Only currently retained
// IDs are kept, bounding pending notifications by the address book's limit.
// Notifications coalesce without waiting for index updates or network requests.
func (p *ProviderHints) PeersAdded(ids []string) {
	if len(ids) == 0 {
		return
	}

	retained := make(map[string]bool)
	for _, id := range p.book.Snapshot().Peers {
		retained[id] = true
	}

	p.addedMu.Lock()
	for id := range p.added {
		if !retained[id] {
			delete(p.added, id)
		}
	}
	for _, id := range ids {
		if retained[id] {
			p.added[id] = true
		}
	}
	p.addedMu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// hintJitter spreads periodic work by +/-20%, clipping the upper bound to the
// largest duration. Integer arithmetic keeps even extreme intervals positive.
func hintJitter(interval time.Duration) time.Duration {
	spread := interval / 5
	low := interval - spread
	high := interval + min(spread, time.Duration(1<<63-1)-interval)
	return low + time.Duration(rand.Int64N(int64(high-low)+1))
}
