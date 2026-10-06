package peering

import (
	"slices"
	"testing"
)

func TestHintIndexFirstReceiptAddsSelfAndStartsPass(t *testing.T) {
	x := newHintIndex(mustNodeID(t, testIDC))
	key := testCacheKey(1)
	x.merge(testClaim(t, testIDA, key, testIDA, testIDB))
	h := x.find(mustNodeID(t, testIDA), key)
	if h == nil {
		t.Fatal("hint not retained")
	}
	if got := seenStrings(h); !slices.Equal(got, []string{testIDA, testIDB, testIDC}) {
		t.Fatalf("seen = %v", got)
	}
	if !h.pending || h.suppressed {
		t.Fatalf("pending = %v, suppressed = %v", h.pending, h.suppressed)
	}
}

func TestHintIndexDuplicateUnionsWithoutReopeningOrRefreshing(t *testing.T) {
	x := newBoundedHintIndex(mustNodeID(t, testIDC), hintLimits{records: 2, cost: maxHintCost, encoded: hintSnapshotBudget})
	first, second, third := testCacheKey(1), testCacheKey(2), testCacheKey(3)
	x.merge(testClaim(t, testIDA, first, testIDA))
	x.merge(testClaim(t, testIDA, second, testIDA))
	h := x.find(mustNodeID(t, testIDA), first)
	h.pending = false // a finished pass

	x.merge(testClaim(t, testIDA, first, testIDA, testIDB, testNodeID(7)))
	if got := seenStrings(h); !slices.Equal(got, []string{testNodeID(7), testIDA, testIDB, testIDC}) {
		t.Fatalf("seen after duplicate = %v", got)
	}
	if h.pending {
		t.Fatal("duplicate reopened a finished pass")
	}
	// The duplicate did not refresh FIFO position, so first is still oldest.
	x.merge(testClaim(t, testIDA, third, testIDA))
	if x.find(mustNodeID(t, testIDA), first) != nil || x.find(mustNodeID(t, testIDA), second) == nil {
		t.Fatal("duplicate refreshed FIFO position")
	}
	if x.evictions != 1 {
		t.Fatalf("evictions = %d", x.evictions)
	}
}

func TestHintIndexIgnoresClaimsOwnedBySelf(t *testing.T) {
	x := newHintIndex(mustNodeID(t, testIDC))
	x.merge(testClaim(t, testIDC, testCacheKey(1), testIDB, testIDC))
	if x.len() != 0 {
		t.Fatalf("retained %d hints owned by self", x.len())
	}
}

func TestHintIndexOwnersIncludesUnknownOwners(t *testing.T) {
	x := newHintIndex(mustNodeID(t, testIDC))
	key, other := testCacheKey(1), testCacheKey(2)
	x.merge(testClaim(t, testNodeID(9), key, testNodeID(9), testIDB))
	x.merge(testClaim(t, testIDA, key, testIDA))
	x.merge(testClaim(t, testIDB, other, testIDB))
	x.originate(key)
	if got := x.owners(key); !slices.Equal(got, []string{testNodeID(9), testIDA, testIDC}) {
		t.Fatalf("owners = %v", got)
	}
	if got := x.owners(testCacheKey(3)); len(got) != 0 {
		t.Fatalf("owners of unknown key = %v", got)
	}
}

func TestHintIndexOriginateAndForgetLocalHints(t *testing.T) {
	self := mustNodeID(t, testIDC)
	x := newHintIndex(self)
	key := testCacheKey(1)
	x.originate(key)
	h := x.find(self, key)
	if h == nil || !h.pending || !slices.Equal(seenStrings(h), []string{testIDC}) {
		t.Fatalf("local hint = %+v", h)
	}
	h.pending = false
	h.seen = []nodeID{mustNodeID(t, testIDA), self}
	x.originate(key)
	if x.find(self, key) != h || h.pending || len(h.seen) != 2 {
		t.Fatal("originating a retained local hint reset it")
	}

	x.merge(testClaim(t, testIDA, key, testIDA))
	x.forget(key)
	if x.find(self, key) != nil {
		t.Fatal("forget kept the local hint")
	}
	if got := x.owners(key); !slices.Equal(got, []string{testIDA}) {
		t.Fatalf("forget changed remote hints: %v", got)
	}
	x.forget(testCacheKey(2)) // absent keys are a no-op
}

func TestHintIndexEvictsWholeRecordsInFIFOOrderUnderCount(t *testing.T) {
	self := mustNodeID(t, testIDC)
	x := newBoundedHintIndex(self, hintLimits{records: 3, cost: maxHintCost, encoded: hintSnapshotBudget})
	x.originate(testCacheKey(1))
	for i := 2; i <= 5; i++ {
		x.merge(testClaim(t, testIDA, testCacheKey(i), testIDA))
	}
	if x.len() != 3 || x.evictions != 2 {
		t.Fatalf("len = %d, evictions = %d", x.len(), x.evictions)
	}
	// Local hints have no protected pool.
	if x.find(self, testCacheKey(1)) != nil || x.find(mustNodeID(t, testIDA), testCacheKey(2)) != nil {
		t.Fatal("oldest records were not evicted first")
	}
	assertHintAccounting(t, x)
}

func TestHintIndexSeenGrowthEvictsOtherRecords(t *testing.T) {
	self := mustNodeID(t, testIDC)
	// Room for three single-receipt records, or one record with two more IDs.
	limit := 3 * hintCost(2)
	x := newBoundedHintIndex(self, hintLimits{records: 10, cost: limit, encoded: hintSnapshotBudget})
	growing := testCacheKey(1)
	x.merge(testClaim(t, testIDA, growing, testIDA))
	x.merge(testClaim(t, testIDA, testCacheKey(2), testIDA))
	x.merge(testClaim(t, testIDA, testCacheKey(3), testIDA))

	seen := []string{testIDA}
	for i := range (hintBaseCost / nodeIDLen) + 1 {
		seen = append(seen, testNodeID(i))
	}
	x.merge(testClaim(t, testIDA, growing, seen...))
	h := x.find(mustNodeID(t, testIDA), growing)
	if h == nil || len(h.seen) != len(seen)+1 {
		t.Fatal("record being updated was evicted or truncated")
	}
	if x.find(mustNodeID(t, testIDA), testCacheKey(2)) != nil {
		t.Fatal("oldest other record survived seen growth")
	}
	if x.cost > limit {
		t.Fatalf("cost %d exceeds limit %d", x.cost, limit)
	}
	// Accounting charges by length, so a union must not retain spare capacity.
	if cap(h.seen) != len(h.seen) {
		t.Fatalf("seen capacity %d exceeds length %d", cap(h.seen), len(h.seen))
	}
	assertHintAccounting(t, x)
}

func TestHintIndexEncodedBudgetEvicts(t *testing.T) {
	x := newBoundedHintIndex(mustNodeID(t, testIDC), hintLimits{records: 10, cost: maxHintCost, encoded: 2 * hintEncodedSize(2)})
	for i := 1; i <= 3; i++ {
		x.merge(testClaim(t, testIDA, testCacheKey(i), testIDA))
	}
	if x.len() != 2 || x.find(mustNodeID(t, testIDA), testCacheKey(1)) != nil {
		t.Fatalf("encoded budget kept %d records", x.len())
	}
	if x.encoded > 2*hintEncodedSize(2) {
		t.Fatalf("encoded %d exceeds budget", x.encoded)
	}
	assertHintAccounting(t, x)
}

func TestHintIndexSeenOverflowSuppressesForwarding(t *testing.T) {
	x := newHintIndex(mustNodeID(t, testIDC))
	key := testCacheKey(1)
	half := []string{testIDA}
	for i := range maxHintSeen / 2 {
		half = append(half, testNodeID(i))
	}
	x.merge(testClaim(t, testIDA, key, half...))
	h := x.find(mustNodeID(t, testIDA), key)
	before := seenStrings(h)

	other := []string{testIDA}
	for i := maxHintSeen / 2; i < maxHintSeen; i++ {
		other = append(other, testNodeID(i))
	}
	x.merge(testClaim(t, testIDA, key, other...))
	if !h.suppressed || h.pending {
		t.Fatalf("suppressed = %v, pending = %v", h.suppressed, h.pending)
	}
	if !slices.Equal(seenStrings(h), before) {
		t.Fatal("overflowing merge changed seen")
	}
	if got := x.owners(key); !slices.Equal(got, []string{testIDA}) {
		t.Fatalf("suppressed hint left lookup: %v", got)
	}
	// Later merges cannot revive a suppressed hint.
	x.merge(testClaim(t, testIDA, key, testIDA, testIDB))
	if !h.suppressed || h.pending || containsNodeID(h.seen, mustNodeID(t, testIDB)) {
		t.Fatal("merge changed a suppressed hint")
	}
	assertHintAccounting(t, x)
}

func TestHintIndexFullClaimWithoutSelfIsSuppressed(t *testing.T) {
	x := newHintIndex(mustNodeID(t, testIDC))
	seen := []string{testIDA}
	for i := range maxHintSeen - 1 {
		seen = append(seen, testNodeID(i))
	}
	x.merge(testClaim(t, testIDA, testCacheKey(1), seen...))
	h := x.find(mustNodeID(t, testIDA), testCacheKey(1))
	if h == nil || !h.suppressed || h.pending || len(h.seen) != maxHintSeen {
		t.Fatalf("full claim = %+v", h)
	}
}

func seenStrings(h *hint) []string {
	seen := make([]string, len(h.seen))
	for i, id := range h.seen {
		seen[i] = id.String()
	}
	return seen
}

func assertHintAccounting(t *testing.T, x *hintIndex) {
	t.Helper()
	var cost, encoded int64
	records := 0
	for e := x.fifo.Front(); e != nil; e = e.Next() {
		h := e.Value.(*hint)
		if x.find(h.owner, h.key) != h {
			t.Fatal("FIFO record missing from key map")
		}
		cost += hintCost(len(h.seen))
		encoded += hintEncodedSize(len(h.seen))
		records++
	}
	mapped := 0
	for _, hints := range x.byKey {
		mapped += len(hints)
	}
	if mapped != records || cost != x.cost || encoded != x.encoded {
		t.Fatalf("mapped=%d records=%d cost=%d/%d encoded=%d/%d", mapped, records, cost, x.cost, encoded, x.encoded)
	}
	if records > x.limits.records || cost > x.limits.cost || encoded > x.limits.encoded {
		t.Fatal("index exceeds its limits")
	}
}
