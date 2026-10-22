package peering

import (
	"context"
	"errors"
)

// QueueRefresh queues a refresh of each peer in ids that is not already
// queued. It waits for neither refreshes nor the hint index, so it suits
// Discovery's newPeers hook. The queue holds at most MaxPeers IDs and drops
// the rest with a warning, and dropped peers are not refreshed.
func (p *ProviderHints) QueueRefresh(ids []string) {
	dropped := 0
	p.queueMu.Lock()
	for _, id := range ids {
		switch {
		case p.queued[id]:
		case len(p.queue) == MaxPeers:
			dropped++
		default:
			p.queue = append(p.queue, id)
			p.queued[id] = true
		}
	}
	p.queueMu.Unlock()
	if dropped != 0 {
		p.logger.Warn("peer cache keys refresh queue full", "rejected", dropped, "limit", MaxPeers)
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run refreshes queued peers one at a time, in queue order, until ctx ends.
// Each queued peer gets one attempt. A peer that has since left the book is
// skipped, and other failures are logged and not retried.
func (p *ProviderHints) Run(ctx context.Context) {
	for ctx.Err() == nil {
		id, ok := p.nextQueued()
		if !ok {
			select {
			case <-ctx.Done():
			case <-p.wake:
			}
			continue
		}
		err := p.Refresh(ctx, id)
		if err != nil && ctx.Err() == nil && !errors.Is(err, errUnknownPeer) {
			p.logger.Warn("peer cache keys refresh failed", "node_id", id)
		}
	}
}

// nextQueued removes and returns the oldest queued ID.
func (p *ProviderHints) nextQueued() (string, bool) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	if len(p.queue) == 0 {
		return "", false
	}
	id := p.queue[0]
	p.queue[0] = ""
	p.queue = p.queue[1:]
	delete(p.queued, id)
	return id, true
}
