package peering

import (
	"bytes"
	"slices"
	"time"
)

// destination is one resolved peer. Backoff is keyed by both parts, so an
// address reused by another node starts fresh while a hint's attempt budget
// does not.
type destination struct {
	id      nodeID
	address string
}

type destinationBackoff struct {
	failures int
	next     time.Time
}

// availabilityBatch is one request in flight with the hints it carries. The
// recipient is credited on acknowledgement, never whichever node answers at
// the address.
type availabilityBatch struct {
	to      destination
	message availabilityMessage
	hints   []*hint
	size    int // encoded bytes
}

// dispatchRound collects the batches one dispatch creates, at most one per
// destination and at most limit in total.
type dispatchRound struct {
	limit   int
	batches []*availabilityBatch
	byID    map[nodeID]*availabilityBatch
	full    int // batches that cannot take another claim
}

// dispatch reserves due work for up to free new requests. For each due hint it
// rechecks the merged seen set against the current resolved book, randomly
// chooses eligible unseen destinations within the hint's remaining fanout and
// attempt budget, and batches claims per destination within the wire bounds.
func (a *Availability) dispatch(free int) []availabilityBatch {
	if free <= 0 {
		return nil
	}
	resolved := a.resolvedPeers()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.pruneBackoff(resolved)
	// When every resolved destination is busy or cooling down, nothing can be
	// sent. Leave due hints queued until a result or backoff expiry rather
	// than rescanning them, which also delays finishing passes that have
	// nothing left to send. An empty book still finishes due passes.
	available := 0
	a.blockedUntil = time.Time{}
	for _, d := range resolved {
		switch {
		case a.busy[d.id]:
		case a.coolingDown(d, now):
			if next := a.backoff[d].next; a.blockedUntil.IsZero() || next.Before(a.blockedUntil) {
				a.blockedUntil = next
			}
		default:
			available++
		}
	}
	if len(resolved) > 0 && available == 0 {
		if a.blockedUntil.IsZero() {
			a.blockedUntil = now.Add(time.Hour) // only a result can free a destination
		}
		return nil
	}
	a.blockedUntil = time.Time{}
	round := &dispatchRound{limit: min(free, max(available, 1)), byID: make(map[nodeID]*availabilityBatch)}
	var retry []*hint
	for range availabilityScanLimit {
		if len(round.batches) == round.limit && round.full == round.limit {
			break
		}
		h := a.index.popDue(now)
		if h == nil {
			break
		}
		if a.plan(h, resolved, round, now) {
			retry = append(retry, h)
		}
	}
	// These hints ran out of request slots, so they wait for a result rather
	// than looping.
	for _, h := range retry {
		a.index.schedule(h, now)
	}
	batches := make([]availabilityBatch, len(round.batches))
	for i, batch := range round.batches {
		a.busy[batch.to.id] = true
		batches[i] = *batch
	}
	return batches
}

// complete applies one request's result. Acknowledgement adds the intended
// recipient to seen and counts it once toward the fanout. A record evicted or
// forgotten meanwhile is not recreated, and its replacement is unaffected.
func (a *Availability) complete(batch availabilityBatch, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if err == nil {
		a.acknowledged++
		delete(a.backoff, batch.to)
	} else {
		a.failed++
		a.recordFailure(batch.to, now)
	}
	a.settle(batch, err == nil, now)
}

// abandon releases a cancelled request without counting it or backing off
// its destination. The attempts it used stay spent.
func (a *Availability) abandon(batch availabilityBatch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settle(batch, false, a.now())
}

// settle requires mu. It releases batch's reservations and advances the
// forwarding pass of every hint it carried that is still retained.
func (a *Availability) settle(batch availabilityBatch, acknowledged bool, now time.Time) {
	delete(a.busy, batch.to.id)
	for _, h := range batch.hints {
		if h.element == nil {
			continue
		}
		h.inflight--
		if acknowledged {
			a.index.addSeen(h, []nodeID{batch.to.id})
		}
		if !h.pending {
			continue
		}
		if acknowledged {
			h.ack(batch.to.id)
		}
		switch {
		case !h.pending:
		case h.ackCount < hintFanout && h.attempts > 0:
			a.index.schedule(h, now.Add(a.jitter(availabilityCadence)))
		case h.inflight == 0:
			a.index.finishPass(h)
		}
	}
}

// plan requires mu. It reserves destinations for h in round and reports
// whether h should retry as soon as a request slot frees. Otherwise h is
// rescheduled for later, finished, or left to its in-flight results.
func (a *Availability) plan(h *hint, resolved []destination, round *dispatchRound, now time.Time) (retry bool) {
	if h.ackCount >= hintFanout || h.attempts == 0 {
		if h.inflight == 0 {
			a.index.finishPass(h)
		}
		return false
	}
	want := min(hintFanout-int(h.ackCount)-int(h.inflight), int(h.attempts))
	if want <= 0 {
		return false
	}
	var eligible []destination
	unseen := 0
	var later time.Time
	for _, d := range resolved {
		if containsNodeID(h.seen, d.id) || h.hasAcked(d.id) {
			continue
		}
		unseen++
		next := time.Time{}
		if a.busy[d.id] {
			next = now.Add(a.jitter(availabilityCadence))
		} else if a.coolingDown(d, now) {
			next = a.backoff[d].next
		} else {
			eligible = append(eligible, d)
			continue
		}
		if later.IsZero() || next.Before(later) {
			later = next
		}
	}
	if unseen == 0 {
		// Every resolved peer has the hint. Discovery does not reopen the pass.
		if h.inflight == 0 {
			a.index.finishPass(h)
		}
		return false
	}
	a.random.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
	blocked := false
	for _, d := range eligible {
		if want == 0 {
			break
		}
		if !a.reserve(round, d, h) {
			blocked = true
			continue
		}
		h.inflight++
		h.attempts--
		want--
	}
	switch {
	case want == 0:
	case blocked:
		return true
	case !later.IsZero():
		a.index.schedule(h, later)
	}
	return false
}

// reserve requires mu. It adds h's claim to the batch for d, creating one if
// round has a free slot, and reports false when the claim cannot fit.
func (a *Availability) reserve(round *dispatchRound, d destination, h *hint) bool {
	batch := round.byID[d.id]
	if batch == nil {
		if len(round.batches) == round.limit {
			return false
		}
		batch = &availabilityBatch{to: d, message: availabilityMessage{sender: a.self, recipient: d.id}, size: availabilityEnvelopeSize}
		round.byID[d.id] = batch
		round.batches = append(round.batches, batch)
	}
	size := claimWireSize(len(h.seen))
	if len(batch.message.claims) > 0 {
		size++ // separator
	}
	if len(batch.message.claims) == maxAvailabilityClaim || batch.size+size > maxAvailabilityBytes {
		return false
	}
	// Seen slices are replaced, never modified, so the batch may share it.
	batch.message.claims = append(batch.message.claims, hintClaim{owner: h.owner, key: h.key, seen: h.seen})
	batch.hints = append(batch.hints, h)
	batch.size += size
	if len(batch.message.claims) == maxAvailabilityClaim || batch.size+claimWireSize(1)+1 > maxAvailabilityBytes {
		round.full++
	}
	return true
}

// resolvedPeers returns the book's identified peers sorted by ID, so seeded
// selection is reproducible. Unresolved candidates are never destinations.
func (a *Availability) resolvedPeers() []destination {
	snapshot := a.book.Snapshot()
	peers := make([]destination, 0, len(snapshot.Peers))
	for address, text := range snapshot.Peers {
		if id, ok := parseNodeID(text); ok && id != a.self {
			peers = append(peers, destination{id: id, address: address})
		}
	}
	slices.SortFunc(peers, func(x, y destination) int { return bytes.Compare(x.id[:], y.id[:]) })
	return peers
}

// pruneBackoff requires mu. It drops backoff for destinations whose ID and
// address pairing is no longer in the book.
func (a *Availability) pruneBackoff(resolved []destination) {
	current := make(map[destination]bool, len(resolved))
	for _, d := range resolved {
		current[d] = true
	}
	for d := range a.backoff {
		if !current[d] {
			delete(a.backoff, d)
		}
	}
}

// coolingDown requires mu.
func (a *Availability) coolingDown(d destination, now time.Time) bool {
	b := a.backoff[d]
	return b != nil && b.next.After(now)
}

// recordFailure requires mu.
func (a *Availability) recordFailure(d destination, now time.Time) {
	b := a.backoff[d]
	if b == nil {
		b = &destinationBackoff{}
		a.backoff[d] = b
	}
	if b.failures < 10 {
		b.failures++
	}
	b.next = now.Add(a.jitter(retryDelay(availabilityCadence, availabilityMaxBackoff, b.failures)))
}

func (h *hint) hasAcked(id nodeID) bool {
	return slices.Contains(h.acked[:h.ackCount], id)
}

func (h *hint) ack(id nodeID) {
	if !h.hasAcked(id) && h.ackCount < hintFanout {
		h.acked[h.ackCount] = id
		h.ackCount++
	}
}

// Node IDs and hex keys contain no characters that JSON escapes, so encoded
// sizes are exact. Each seen ID adds its quoted text and a separator, less
// one separator for the first. Valid claims always have a seen ID.
var availabilityEnvelopeSize = len(`{"senderNodeId":"","recipientNodeId":"","claims":[]}`) + 2*nodeIDLen

func claimWireSize(seen int) int {
	return len(`{"ownerNodeId":"","key":"","seen":[]}`) + nodeIDLen + 2*cacheKeyLen + seen*(nodeIDLen+3) - 1
}
