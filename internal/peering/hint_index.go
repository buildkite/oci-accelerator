package peering

import (
	"container/heap"
	"container/list"
	"slices"
	"time"
)

const (
	maxHintRecords = 200_000
	maxHintCost    = 64 << 20
	// maxHintSeen admits every node a full address book can describe.
	maxHintSeen = MaxPeers + 1
	// maxHintSnapshot bounds the encoded snapshot file. The index
	// keeps its records within this less a fixed allowance for the header.
	maxHintSnapshot    = 64 << 20
	hintSnapshotBudget = maxHintSnapshot - 4<<10

	// hintBaseCost estimates one record's memory apart from its seen IDs: the
	// record itself with its inline forwarding state, its share of the key
	// map, its FIFO element, its due-queue slot, and allocator slack.
	hintBaseCost = 384
	// hintEncodedBase and hintEncodedPerSeen bound one record's snapshot
	// encoding. A seen ID costs its quoted text and a separator.
	hintEncodedBase    = 256
	hintEncodedPerSeen = nodeIDLen + 3
)

// hintIndex is the bounded directory of provider hints, keyed by provider and
// cache key. Local and remote hints share one FIFO, and admission or seen
// growth evicts the oldest whole records to stay within every limit.
// Duplicates never refresh FIFO position. The caller serializes access.
type hintIndex struct {
	self      nodeID
	limits    hintLimits
	byKey     map[cacheKey][]*hint
	fifo      list.List // *hint, oldest first
	queue     hintQueue // pending hints awaiting dispatch, earliest due first
	cost      int64     // accounted memory of retained records
	encoded   int64     // accounted worst-case snapshot bytes of retained records
	evictions uint64    // records removed to make room
}

type hintLimits struct {
	records       int
	cost, encoded int64
}

// hint records that provider held key. Seen lists the nodes known to have
// received it. A suppressed hint's seen set could not hold a complete union,
// so it remains available for lookup but is never forwarded again. Its seen
// set may lack the local ID when the first receipt was already full.
//
// A pending hint is in its forwarding pass. The pass counts distinct
// acknowledged destinations, requests in flight, and dispatches left, and it
// finishes at the fanout, when attempts run out, or when no resolved peer is
// unseen. A finished pass never reopens while the record survives.
type hint struct {
	provider   nodeID
	key        cacheKey
	seen       []nodeID // sorted, unique
	suppressed bool
	pending    bool
	acked      [hintFanout]nodeID
	ackCount   uint8
	inflight   uint8
	attempts   uint8
	due        time.Time
	queued     int // position in the due queue, or -1
	element    *list.Element
}

// announcedHint is one validated hint from an announcement.
type announcedHint struct {
	provider nodeID
	key      cacheKey
	seen     []nodeID // sorted, unique, contains provider and sender
}

func newHintIndex(self nodeID) *hintIndex {
	return newBoundedHintIndex(self, hintLimits{records: maxHintRecords, cost: maxHintCost, encoded: hintSnapshotBudget})
}

func newBoundedHintIndex(self nodeID, limits hintLimits) *hintIndex {
	return &hintIndex{self: self, limits: limits, byKey: make(map[cacheKey][]*hint)}
}

// merge records a received hint and reports whether retained state changed.
// The local node's own hints are ignored because its cache inventory is
// authoritative. A first receipt adds the local ID and starts a forwarding
// pass due at due. A duplicate only unions seen sets, without refreshing FIFO
// position, moving its deadline, or reopening a finished pass.
func (x *hintIndex) merge(c announcedHint, due time.Time) bool {
	if c.provider == x.self {
		return false
	}
	h := x.find(c.provider, c.key)
	if h == nil {
		h = &hint{provider: c.provider, key: c.key, queued: -1}
		h.seen = unionNodeIDs(c.seen, []nodeID{x.self})
		if len(h.seen) > maxHintSeen {
			h.seen = slices.Clone(c.seen)
			h.suppressed = true
		}
		x.add(h)
		if !h.suppressed {
			x.startPass(h, due)
		}
		return true
	}
	return x.addSeen(h, c.seen)
}

// addSeen unions ids into a retained hint's seen set and reports whether it
// changed. A union that would exceed the seen limit suppresses the hint
// instead, because truncating seen and relaying could resend to nodes that
// already have the hint.
func (x *hintIndex) addSeen(h *hint, ids []nodeID) bool {
	if h.suppressed {
		return false
	}
	missing := countMissing(h.seen, ids)
	switch {
	case missing == 0:
		return false
	case len(h.seen)+missing > maxHintSeen:
		h.suppressed = true
		x.finishPass(h)
	default:
		x.setSeen(h, unionNodeIDs(h.seen, ids))
	}
	return true
}

// announce starts a local hint for key due at due unless one is already
// retained, so a retained record keeps its seen set and forwarding progress.
func (x *hintIndex) announce(key cacheKey, due time.Time) {
	if x.find(x.self, key) == nil {
		h := &hint{provider: x.self, key: key, seen: []nodeID{x.self}, queued: -1}
		x.add(h)
		x.startPass(h, due)
	}
}

// forget removes the local hint for key, without any withdrawal, and reports
// whether one was retained.
func (x *hintIndex) forget(key cacheKey) bool {
	h := x.find(x.self, key)
	if h != nil {
		x.remove(h)
	}
	return h != nil
}

// providers returns the sorted node IDs advertised as holding key. Hints are
// advisory and may be stale.
func (x *hintIndex) providers(key cacheKey) []string {
	hints := x.byKey[key]
	providers := make([]string, 0, len(hints))
	for _, h := range hints {
		providers = append(providers, h.provider.String())
	}
	slices.Sort(providers)
	return providers
}

func (x *hintIndex) len() int { return x.fifo.Len() }

func (x *hintIndex) startPass(h *hint, due time.Time) {
	h.pending = true
	h.attempts = hintAttempts
	x.schedule(h, due)
}

// finishPass ends the forwarding pass. Requests already in flight still
// report their results.
func (x *hintIndex) finishPass(h *hint) {
	h.pending = false
	x.unschedule(h)
}

// schedule queues a pending hint for dispatch at due, keeping an earlier
// deadline when it is already queued.
func (x *hintIndex) schedule(h *hint, due time.Time) {
	if h.queued < 0 {
		h.due = due
		heap.Push(&x.queue, h)
	} else if due.Before(h.due) {
		h.due = due
		heap.Fix(&x.queue, h.queued)
	}
}

func (x *hintIndex) unschedule(h *hint) {
	if h.queued >= 0 {
		heap.Remove(&x.queue, h.queued)
	}
}

// popDue dequeues the earliest hint due by now, or returns nil.
func (x *hintIndex) popDue(now time.Time) *hint {
	if len(x.queue) == 0 || x.queue[0].due.After(now) {
		return nil
	}
	return heap.Pop(&x.queue).(*hint)
}

// nextDue reports the earliest queued deadline.
func (x *hintIndex) nextDue() (time.Time, bool) {
	if len(x.queue) == 0 {
		return time.Time{}, false
	}
	return x.queue[0].due, true
}

func (x *hintIndex) find(provider nodeID, key cacheKey) *hint {
	for _, h := range x.byKey[key] {
		if h.provider == provider {
			return h
		}
	}
	return nil
}

func (x *hintIndex) add(h *hint) {
	h.element = x.fifo.PushBack(h)
	x.byKey[h.key] = append(x.byKey[h.key], h)
	x.cost += hintCost(len(h.seen))
	x.encoded += hintEncodedSize(len(h.seen))
	x.makeRoom(h)
}

func (x *hintIndex) setSeen(h *hint, seen []nodeID) {
	x.cost += hintCost(len(seen)) - hintCost(len(h.seen))
	x.encoded += hintEncodedSize(len(seen)) - hintEncodedSize(len(h.seen))
	h.seen = seen
	x.makeRoom(h)
}

// makeRoom evicts the oldest records other than keep until every limit holds.
// Keeping the record being updated avoids discarding it halfway through. Only
// limits smaller than one record could leave keep alone over a limit.
func (x *hintIndex) makeRoom(keep *hint) {
	for x.fifo.Len() > x.limits.records || x.cost > x.limits.cost || x.encoded > x.limits.encoded {
		oldest := x.fifo.Front()
		if oldest != nil && oldest.Value == keep {
			oldest = oldest.Next()
		}
		if oldest == nil {
			return
		}
		x.remove(oldest.Value.(*hint))
		x.evictions++
	}
}

// remove forgets h with its forwarding state. A nil element tells requests
// already in flight that their record is gone.
func (x *hintIndex) remove(h *hint) {
	x.unschedule(h)
	x.fifo.Remove(h.element)
	h.element = nil
	hints := slices.DeleteFunc(x.byKey[h.key], func(other *hint) bool { return other == h })
	if len(hints) == 0 {
		delete(x.byKey, h.key)
	} else {
		x.byKey[h.key] = hints
	}
	x.cost -= hintCost(len(h.seen))
	x.encoded -= hintEncodedSize(len(h.seen))
}

func hintCost(seen int) int64 { return hintBaseCost + int64(seen)*nodeIDLen }

func hintEncodedSize(seen int) int64 {
	return hintEncodedBase + int64(seen)*hintEncodedPerSeen
}

// hintQueue is a min-heap of pending hints ordered by due time.
type hintQueue []*hint

func (q hintQueue) Len() int           { return len(q) }
func (q hintQueue) Less(i, j int) bool { return q[i].due.Before(q[j].due) }
func (q hintQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].queued, q[j].queued = i, j
}

func (q *hintQueue) Push(value any) {
	h := value.(*hint)
	h.queued = len(*q)
	*q = append(*q, h)
}

func (q *hintQueue) Pop() any {
	old := *q
	h := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	h.queued = -1
	return h
}
