package peering

import (
	"container/list"
	"slices"
)

const (
	maxHintRecords = 200_000
	maxHintCost    = 64 << 20
	// maxHintSeen admits every node a full address book can describe.
	maxHintSeen = MaxPeers + 1
	// maxAvailabilitySnapshot bounds the encoded snapshot file. The index
	// keeps its records within this less a fixed allowance for the header.
	maxAvailabilitySnapshot = 64 << 20
	hintSnapshotBudget      = maxAvailabilitySnapshot - 4<<10

	// hintBaseCost estimates one record's memory apart from its seen IDs: the
	// record itself, its share of the key map, its FIFO element, and its
	// forwarding and scheduling state.
	hintBaseCost = 320
	// hintEncodedBase and hintEncodedPerSeen bound one record's snapshot
	// encoding. A seen ID costs its quoted text and a separator.
	hintEncodedBase    = 256
	hintEncodedPerSeen = nodeIDLen + 3
)

// hintIndex is the bounded directory of availability hints, keyed by owner and
// cache key. Local and remote hints share one FIFO, and admission or seen
// growth evicts the oldest whole records to stay within every limit.
// Duplicates never refresh FIFO position. The caller serializes access.
type hintIndex struct {
	self      nodeID
	limits    hintLimits
	byKey     map[cacheKey][]*hint
	fifo      list.List // *hint, oldest first
	cost      int64     // accounted memory of retained records
	encoded   int64     // accounted worst-case snapshot bytes of retained records
	evictions uint64    // records removed to make room
}

type hintLimits struct {
	records       int
	cost, encoded int64
}

// hint records that owner held key. Seen lists the nodes known to have
// received it. A suppressed hint's seen set could not hold a complete union,
// so it remains available for lookup but is never forwarded again. Its seen
// set may lack the local ID when the first receipt was already full.
type hint struct {
	owner      nodeID
	key        cacheKey
	seen       []nodeID // sorted, unique
	suppressed bool
	pending    bool // the forwarding pass has not finished
	element    *list.Element
}

// hintClaim is one validated wire claim.
type hintClaim struct {
	owner nodeID
	key   cacheKey
	seen  []nodeID // sorted, unique, contains owner and sender
}

func newHintIndex(self nodeID) *hintIndex {
	return newBoundedHintIndex(self, hintLimits{records: maxHintRecords, cost: maxHintCost, encoded: hintSnapshotBudget})
}

func newBoundedHintIndex(self nodeID, limits hintLimits) *hintIndex {
	return &hintIndex{self: self, limits: limits, byKey: make(map[cacheKey][]*hint)}
}

// merge records a received claim. The local node's own claims are ignored
// because its cache inventory is authoritative. A first receipt adds the local
// ID and starts a forwarding pass. A duplicate only unions seen sets, without
// refreshing FIFO position or reopening a finished pass.
func (x *hintIndex) merge(c hintClaim) {
	if c.owner == x.self {
		return
	}
	h := x.find(c.owner, c.key)
	if h == nil {
		h = &hint{owner: c.owner, key: c.key, pending: true}
		h.seen = unionNodeIDs(c.seen, []nodeID{x.self})
		if len(h.seen) > maxHintSeen {
			h.seen = slices.Clone(c.seen)
			h.suppressed, h.pending = true, false
		}
		x.add(h)
		return
	}
	if h.suppressed {
		return
	}
	missing := countMissing(h.seen, c.seen)
	switch {
	case missing == 0:
	case len(h.seen)+missing > maxHintSeen:
		// Never truncate seen and keep relaying, which could resend to nodes
		// that already have the hint.
		h.suppressed, h.pending = true, false
	default:
		x.setSeen(h, unionNodeIDs(h.seen, c.seen))
	}
}

// originate starts a local hint for key unless one is already retained, so a
// retained record keeps its seen set and forwarding progress.
func (x *hintIndex) originate(key cacheKey) {
	if x.find(x.self, key) == nil {
		x.add(&hint{owner: x.self, key: key, seen: []nodeID{x.self}, pending: true})
	}
}

// forget removes the local hint for key, without any withdrawal.
func (x *hintIndex) forget(key cacheKey) {
	if h := x.find(x.self, key); h != nil {
		x.remove(h)
	}
}

// owners returns the sorted node IDs advertised as holding key. Hints are
// advisory and may be stale.
func (x *hintIndex) owners(key cacheKey) []string {
	hints := x.byKey[key]
	owners := make([]string, 0, len(hints))
	for _, h := range hints {
		owners = append(owners, h.owner.String())
	}
	slices.Sort(owners)
	return owners
}

func (x *hintIndex) len() int { return x.fifo.Len() }

func (x *hintIndex) find(owner nodeID, key cacheKey) *hint {
	for _, h := range x.byKey[key] {
		if h.owner == owner {
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

func (x *hintIndex) remove(h *hint) {
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
