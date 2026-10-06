package peering

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	testOwner = "owner.test:1"
	testPeerA = "a.test:1"
	testPeerB = "b.test:1"
)

var testNow = time.UnixMilli(1_700_000_000_000)

func TestAvailabilityIndexMergeRejectsInvalidBatches(t *testing.T) {
	valid := testClaim(1, 1, true, testPeerA, testOwner)
	tests := map[string]struct {
		mutate func(*Claim)
		sender string
		self   string
	}{
		"sender not canonical": {sender: "A.test:1"},
		"self not canonical":   {self: "self.test"},
		"sender not seen":      {sender: testPeerB},
		"owner not seen":       {mutate: func(c *Claim) { c.Seen = []string{testPeerA} }},
		"seen empty":           {mutate: func(c *Claim) { c.Seen = nil }},
		"seen unsorted":        {mutate: func(c *Claim) { c.Seen = []string{testOwner, testPeerA} }},
		"seen duplicate":       {mutate: func(c *Claim) { c.Seen = []string{testPeerA, testPeerA, testOwner} }},
		"seen not canonical":   {mutate: func(c *Claim) { c.Seen = []string{"Z.test:1", testPeerA, testOwner} }},
		"seen too large":       {mutate: func(c *Claim) { c.Seen = testAddresses(maxSeen+1, testPeerA, testOwner) }},
		"owner not canonical":  {mutate: func(c *Claim) { c.Owner = "Owner.test:1" }},
		"key not hex":          {mutate: func(c *Claim) { c.Key = c.Key[:63] + "g" }},
		"key upper":            {mutate: func(c *Claim) { c.Key = fmt.Sprintf("%064X", 0xabc) }},
		"session short":        {mutate: func(c *Claim) { c.Session = c.Session[:31] }},
		"revision zero":        {mutate: func(c *Claim) { c.Revision = 0 }},
		"expiry not positive":  {mutate: func(c *Claim) { c.Expires = 0 }},
		"expiry beyond lease":  {mutate: func(c *Claim) { c.Expires = testNow.Add(availabilityLease+availabilitySkew).UnixMilli() + 1 }},
		"expiry negative":      {mutate: func(c *Claim) { c.Expires = -1 }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			index := newAvailabilityIndex()
			bad := copyClaim(valid)
			bad.Key = testKey(2)
			if test.mutate != nil {
				test.mutate(&bad)
			}
			sender, self := testPeerA, testSelf
			if test.sender != "" {
				sender = test.sender
			}
			if test.self != "" {
				self = test.self
			}
			if _, err := index.merge([]Claim{valid, bad}, self, sender, testNow); err == nil {
				t.Fatal("merge accepted invalid batch")
			}
			if index.count != 0 || len(index.owners(valid.Key, testNow)) != 0 {
				t.Fatal("invalid batch partially applied")
			}
		})
	}

	index := newAvailabilityIndex()
	claims := make([]Claim, maxClaimBatch+1)
	for i := range claims {
		claims[i] = testClaim(i, 1, true, testPeerA, testOwner)
	}
	if _, err := index.merge(claims, testSelf, testPeerA, testNow); err == nil {
		t.Fatal("merge accepted oversized batch")
	}
	edge := testClaim(1, 1, true, testPeerA, testOwner)
	edge.Expires = testNow.Add(availabilityLease + availabilitySkew).UnixMilli()
	if _, err := index.merge(append(claims[:maxClaimBatch-1], edge), testSelf, testPeerA, testNow); err != nil {
		t.Fatalf("merge rejected maximal batch: %v", err)
	}
}

func TestAvailabilityIndexMergeSkipsExpiredAndSelfOwned(t *testing.T) {
	index := newAvailabilityIndex()
	expired := testClaim(1, 1, true, testPeerA, testOwner)
	expired.Expires = testNow.UnixMilli()
	selfOwned := testClaim(2, 1, true, testPeerA, testSelf)
	selfOwned.Owner = testSelf
	mustMerge(t, index, testPeerA, expired, selfOwned)
	if index.count != 0 || len(index.owners(testKey(2), testNow)) != 0 {
		t.Fatalf("stored %d expired or self-owned claims", index.count)
	}
}

func TestAvailabilityIndexBatchFiltersSeenAddresses(t *testing.T) {
	index := newAvailabilityIndex()
	claim := testClaim(1, 1, true, testPeerA, testOwner)
	mustMerge(t, index, testPeerA, claim)

	for _, destination := range []string{testPeerA, testOwner, testSelf} {
		if got := index.batch(destination, testNow); len(got) != 0 {
			t.Fatalf("batch to %s returned seen claim %+v", destination, got)
		}
	}
	got := index.batch(testPeerB, testNow)
	want := copyClaim(claim)
	want.Seen = []string{testPeerA, testOwner, testSelf}
	if len(got) != 1 || !claimEqual(got[0], want) {
		t.Fatalf("batch to B = %+v, want %+v", got, want)
	}
}

func TestAvailabilityIndexEqualRevisionUnionsSeen(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))
	mustMerge(t, index, testPeerB, testClaim(1, 1, true, testPeerB, testOwner))

	if got := index.batch(testPeerB, testNow); len(got) != 0 {
		t.Fatalf("batch to B after B relayed it = %+v", got)
	}
	got := index.batch("c.test:1", testNow)
	if len(got) != 1 || !slices.Equal(got[0].Seen, []string{testPeerA, testPeerB, testOwner, testSelf}) {
		t.Fatalf("union seen = %+v", got)
	}
}

func TestAvailabilityIndexEqualRevisionConflictKeepsSeen(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))
	conflictExpiry := testClaim(1, 1, true, testPeerB, testOwner)
	conflictExpiry.Expires--
	conflictPresent := testClaim(1, 1, false, testPeerB, testOwner)
	mustMerge(t, index, testPeerB, conflictExpiry, conflictPresent)

	got := index.batch(testPeerB, testNow)
	if len(got) != 1 || !got[0].Present || got[0].Expires != testClaim(1, 1, true).Expires {
		t.Fatalf("conflicting equal revision changed claim: %+v", got)
	}
}

func TestAvailabilityIndexNewerRevisionResetsSeen(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testPeerB, testOwner))
	if got := index.batch(testPeerB, testNow); len(got) != 0 {
		t.Fatalf("B already saw revision 1: %+v", got)
	}
	mustMerge(t, index, testPeerA, testClaim(1, 2, false, testPeerA, testOwner))

	got := index.batch(testPeerB, testNow)
	if len(got) != 1 || got[0].Revision != 2 || got[0].Present || !slices.Equal(got[0].Seen, []string{testPeerA, testOwner, testSelf}) {
		t.Fatalf("newer revision did not reset seen: %+v", got)
	}
}

func TestAvailabilityIndexNewerRevisionCannotShortenExpiry(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))
	shorter := testClaim(1, 2, false, testPeerA, testOwner)
	shorter.Expires--
	mustMerge(t, index, testPeerA, shorter)

	if got := index.owners(testKey(1), testNow); !slices.Equal(got, []string{testOwner}) {
		t.Fatalf("shorter newer revision replaced positive: owners %v", got)
	}
}

func TestAvailabilityIndexBatchPreservesClaimAndCopiesSeen(t *testing.T) {
	index := newAvailabilityIndex()
	claim := testClaim(1, 7, true, testPeerA, testOwner)
	claim.Expires = testNow.Add(3 * time.Minute).UnixMilli()
	mustMerge(t, index, testPeerA, claim)
	claim.Seen[0] = "mutated.test:1" // the index must not alias merge input

	first := index.batch(testPeerB, testNow)
	if len(first) != 1 {
		t.Fatalf("batch = %+v", first)
	}
	got := first[0]
	if got.Owner != testOwner || got.Session != testSession(1) || got.Key != testKey(1) || got.Revision != 7 || !got.Present || got.Expires != claim.Expires {
		t.Fatalf("relay changed original fields: %+v", got)
	}
	got.Seen[0] = "mutated.test:1"
	second := index.batch(testPeerB, testNow)
	if len(second) != 1 || !slices.Equal(second[0].Seen, []string{testPeerA, testOwner, testSelf}) {
		t.Fatalf("index seen aliased: %+v", second)
	}
}

func TestAvailabilityIndexUnacknowledgedClaimsAreResent(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))

	first := index.batch(testPeerB, testNow)
	// A failed delivery never calls ack, so the claim is offered again.
	second := index.batch(testPeerB, testNow)
	if len(first) != 1 || len(second) != 1 || !claimEqual(first[0], second[0]) {
		t.Fatalf("unacknowledged claim not resent: %+v then %+v", first, second)
	}
	index.ack(testPeerB, second)
	if got := index.batch(testPeerB, testNow); len(got) != 0 {
		t.Fatalf("acknowledged claim resent: %+v", got)
	}
	if got := index.batch("c.test:1", testNow); len(got) != 1 || !slices.Equal(got[0].Seen, []string{testPeerA, testPeerB, testOwner, testSelf}) {
		t.Fatalf("ack did not record destination: %+v", got)
	}
}

func TestAvailabilityIndexDelayedAckSkipsLaterRevision(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))
	sent := index.batch(testPeerB, testNow)
	mustMerge(t, index, testPeerA, testClaim(1, 2, false, testPeerA, testOwner))

	index.ack(testPeerB, sent)
	got := index.batch(testPeerB, testNow)
	if len(got) != 1 || got[0].Revision != 2 {
		t.Fatalf("delayed ack suppressed later revision: %+v", got)
	}
}

func TestAvailabilityIndexAckRequiresExactPayload(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 1, true, testPeerA, testOwner))
	sent := index.batch(testPeerB, testNow)
	wrongExpiry := copyClaim(sent[0])
	wrongExpiry.Expires--
	wrongPresent := copyClaim(sent[0])
	wrongPresent.Present = false
	unknown := copyClaim(sent[0])
	unknown.Session = testSession(9)

	index.ack(testPeerB, []Claim{wrongExpiry, wrongPresent, unknown})
	index.ack("B.test:1", sent) // not canonical
	if got := index.batch(testPeerB, testNow); len(got) != 1 {
		t.Fatalf("mismatched ack recorded destination: %+v", got)
	}
}

func TestAvailabilityIndexWithdrawalBlocksReorderedPositive(t *testing.T) {
	index := newAvailabilityIndex()
	mustMerge(t, index, testPeerA, testClaim(1, 2, false, testPeerA, testOwner))
	mustMerge(t, index, testPeerB, testClaim(1, 1, true, testPeerB, testOwner))

	if got := index.owners(testKey(1), testNow); len(got) != 0 {
		t.Fatalf("reordered positive resurrected owner: %v", got)
	}
	got := index.batch("c.test:1", testNow)
	if len(got) != 1 || got[0].Present || got[0].Revision != 2 || !slices.Equal(got[0].Seen, []string{testPeerA, testOwner, testSelf}) {
		t.Fatalf("withdrawal changed by older positive: %+v", got)
	}
}

func TestAvailabilityIndexBootSessionsCoexist(t *testing.T) {
	index := newAvailabilityIndex()
	old := testClaim(1, 9, false, testPeerA, testOwner)
	restarted := testClaim(1, 1, true, testPeerA, testOwner)
	restarted.Session = testSession(2)
	other := testClaim(1, 1, true, testPeerA, testPeerB)
	other.Owner = testPeerB
	mustMerge(t, index, testPeerA, old, restarted, other)

	if got := index.owners(testKey(1), testNow); !slices.Equal(got, []string{testPeerB, testOwner}) {
		t.Fatalf("owners = %v", got)
	}
	restarted.Revision = 2
	restarted.Present = false
	mustMerge(t, index, testPeerA, restarted)
	if got := index.owners(testKey(1), testNow); !slices.Equal(got, []string{testPeerB}) {
		t.Fatalf("owners after withdrawal = %v", got)
	}
	if got := index.batch("c.test:1", testNow); len(got) != 3 {
		t.Fatalf("batch = %d claims, want both sessions and other owner", len(got))
	}
}

func TestAvailabilityIndexRecordCapacityDropsNewEntries(t *testing.T) {
	index := newAvailabilityIndex()
	index.maxRecords = 2
	tombstone := testClaim(1, 1, false, testPeerA, testOwner)
	dropped := mustMerge(t, index, testPeerA, tombstone, testClaim(2, 1, true, testPeerA, testOwner), testClaim(3, 1, true, testPeerA, testOwner))
	if dropped != 1 || index.count != 2 {
		t.Fatalf("dropped %d with %d records, want 1 and 2", dropped, index.count)
	}
	if got := index.owners(testKey(3), testNow); len(got) != 0 {
		t.Fatalf("over-capacity claim stored: %v", got)
	}
	// Existing records still accept newer revisions at capacity.
	if dropped := mustMerge(t, index, testPeerA, testClaim(2, 2, false, testPeerA, testOwner)); dropped != 0 {
		t.Fatalf("update at capacity dropped %d", dropped)
	}
	if got := index.owners(testKey(2), testNow); len(got) != 0 {
		t.Fatalf("withdrawal at capacity ignored: %v", got)
	}
	// Local records share the capacity and never evict tombstones.
	if dropped := index.syncLocal(testSelf, testSession(5), []string{testKey(4)}, false, testNow); dropped != 1 {
		t.Fatalf("local sync at capacity dropped %d, want 1", dropped)
	}
	if entry := index.keys[testKey(1)][claimOwner{testOwner, testSession(1)}]; entry == nil || entry.claim.Present {
		t.Fatal("tombstone evicted at capacity")
	}
}

func TestAvailabilityIndexByteBudgetRefusesGrowth(t *testing.T) {
	index := newAvailabilityIndex()
	first := testClaim(1, 1, true, testPeerA, testOwner)
	mustMerge(t, index, testPeerA, first)
	index.maxBytes = index.bytes

	if dropped := mustMerge(t, index, testPeerA, testClaim(2, 1, true, testPeerA, testOwner)); dropped != 1 {
		t.Fatalf("new record beyond byte budget dropped %d", dropped)
	}
	sent := index.batch(testPeerB, testNow)
	index.ack(testPeerB, sent)
	if got := index.batch(testPeerB, testNow); len(got) != 0 {
		t.Fatal("acknowledged claim relayed after its seen set exhausted the budget")
	}
	if dropped := mustMerge(t, index, testPeerB, testClaim(1, 1, true, testPeerB, testOwner)); dropped != 1 {
		t.Fatalf("seen union beyond byte budget dropped %d", dropped)
	}
	if got := index.batch("c.test:1", testNow); len(got) != 0 {
		t.Fatal("claim relayed after refusing seen growth")
	}
	if index.bytes > index.maxBytes {
		t.Fatalf("bytes %d exceed budget %d", index.bytes, index.maxBytes)
	}

	// Retain a newer withdrawal at capacity but do not relay without its seen set.
	newer := testClaim(1, 2, false, testPeerA, testPeerB, "c.test:1", testOwner)
	if dropped := mustMerge(t, index, testPeerA, newer); dropped != 1 {
		t.Fatalf("oversized newer revision dropped %d, want 1", dropped)
	}
	if got := index.batch("d.test:1", testNow); len(got) != 0 {
		t.Fatalf("relayed newer revision without full seen set: %+v", got)
	}
	if got := index.owners(first.Key, testNow); len(got) != 0 {
		t.Fatalf("withdrawal was lost at capacity: %v", got)
	}
}

func TestAvailabilityIndexByteAccountingIncludesSeen(t *testing.T) {
	index := newAvailabilityIndex()
	small := testClaim(1, 1, true, testPeerA, testOwner)
	mustMerge(t, index, testPeerA, small)
	smallBytes := index.bytes
	large := testClaim(2, 1, true, testAddresses(100, testPeerA, testOwner)...)
	mustMerge(t, index, testPeerA, large)
	if delta := index.bytes - smallBytes; delta <= smallBytes+100*claimSeenOverhead {
		t.Fatalf("large seen set accounted %d bytes, small claim %d", delta, smallBytes)
	}
	index.expire(testNow.Add(time.Hour))
	if index.bytes != 0 || index.count != 0 || len(index.keys) != 0 {
		t.Fatalf("after expiry bytes=%d count=%d keys=%d", index.bytes, index.count, len(index.keys))
	}
}

func TestAvailabilityIndexFullSeenSetsAreNotRelayed(t *testing.T) {
	index := newAvailabilityIndex()
	full := testClaim(1, 1, true, testAddresses(maxSeen, testPeerA, testOwner)...)
	nearlyFull := testClaim(2, 1, true, testAddresses(maxSeen-1, testPeerA, testOwner)...)
	mustMerge(t, index, testPeerA, full, nearlyFull)

	if got := index.owners(testKey(1), testNow); !slices.Equal(got, []string{testOwner}) {
		t.Fatalf("full seen claim not retained: %v", got)
	}
	if got := index.keys[testKey(1)][claimOwner{testOwner, testSession(1)}].claim.Seen; containsAddress(got, testSelf) || len(got) != maxSeen {
		t.Fatalf("full seen set changed: %d addresses", len(got))
	}
	if got := index.keys[testKey(2)][claimOwner{testOwner, testSession(1)}].claim.Seen; !containsAddress(got, testSelf) || len(got) != maxSeen {
		t.Fatalf("self not added to nearly full seen set: %d addresses", len(got))
	}
	if got := index.batch(testPeerB, testNow); len(got) != 0 {
		t.Fatalf("full seen sets relayed: %d claims", len(got))
	}
}

func TestAvailabilityIndexAckCanFillSeenSet(t *testing.T) {
	index := newAvailabilityIndex()
	claim := testClaim(1, 1, true, testAddresses(maxSeen-2, testPeerA, testOwner)...)
	mustMerge(t, index, testPeerA, claim)
	sent := index.batch(testPeerB, testNow)
	if len(sent) != 1 {
		t.Fatalf("batch = %d claims", len(sent))
	}
	index.ack(testPeerB, sent)
	if got := index.batch("zz.test:1", testNow); len(got) != 0 {
		t.Fatal("seen set filled by ack still relayed")
	}
}

func TestAvailabilityIndexBatchesProgressAcrossCalls(t *testing.T) {
	index := newAvailabilityIndex()
	const total = 3*maxClaimBatch - 7
	for start := 0; start < total; start += maxClaimBatch {
		var claims []Claim
		for i := start; i < min(start+maxClaimBatch, total); i++ {
			claims = append(claims, testClaim(i, 1, true, testPeerA, testOwner))
		}
		mustMerge(t, index, testPeerA, claims...)
	}

	seen := map[string]bool{}
	for range 3 {
		got := index.batch(testPeerB, testNow)
		if len(got) > maxClaimBatch {
			t.Fatalf("batch of %d claims", len(got))
		}
		for _, claim := range got {
			seen[claim.Key] = true
		}
	}
	if len(seen) != total {
		t.Fatalf("three batches offered %d of %d claims", len(seen), total)
	}
	// Destinations scan independently.
	if got := index.batch("c.test:1", testNow); len(got) != maxClaimBatch || got[0].Key != testKey(0) {
		t.Fatalf("independent destination batch = %+v", got)
	}
}

func TestAvailabilityIndexBatchBoundsEncodedBytes(t *testing.T) {
	index := newAvailabilityIndex()
	const total = 40
	for i := range total {
		mustMerge(t, index, testPeerA, testClaim(i, 1, true, testAddresses(1000, testPeerA, testOwner)...))
	}

	seen := map[string]bool{}
	for calls := 0; len(seen) < total; calls++ {
		if calls == total {
			t.Fatalf("only %d of %d claims offered", len(seen), total)
		}
		got := index.batch(testPeerB, testNow)
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || len(got) == total || len(encoded) > claimBatchBytes {
			t.Fatalf("batch of %d claims encoded to %d bytes", len(got), len(encoded))
		}
		for _, claim := range got {
			seen[claim.Key] = true
		}
		index.ack(testPeerB, got)
	}
}

func TestAvailabilityIndexBatchScanIsBounded(t *testing.T) {
	index := newAvailabilityIndex()
	total := maxClaimScan*2 + 10
	for start := 0; start < total; start += maxClaimBatch {
		var claims []Claim
		for i := start; i < min(start+maxClaimBatch, total); i++ {
			seen := []string{testPeerA, testPeerB, testOwner}
			if i == total-1 {
				seen = []string{testPeerA, testOwner}
			}
			claims = append(claims, testClaim(i, 1, true, seen...))
		}
		mustMerge(t, index, testPeerA, claims...)
	}

	for call := range 2 {
		if got := index.batch(testPeerB, testNow); len(got) != 0 {
			t.Fatalf("call %d scanned past its bound: %+v", call, got)
		}
	}
	if got := index.batch(testPeerB, testNow); len(got) != 1 || got[0].Key != testKey(total-1) {
		t.Fatalf("scan did not reach last claim: %+v", got)
	}
}

func TestAvailabilityIndexExpiryCleanup(t *testing.T) {
	index := newAvailabilityIndex()
	short := testClaim(1, 1, true, testPeerA, testOwner)
	short.Expires = testNow.Add(time.Minute).UnixMilli()
	long := testClaim(2, 1, false, testPeerA, testOwner)
	mustMerge(t, index, testPeerA, short, long)

	later := testNow.Add(time.Minute)
	if got := index.owners(testKey(1), later); len(got) != 0 {
		t.Fatalf("expired owner returned: %v", got)
	}
	if got := index.batch(testPeerB, later); len(got) != 1 || got[0].Key != testKey(2) {
		t.Fatalf("batch after expiry = %+v", got)
	}
	if index.count != 1 {
		t.Fatalf("batch left %d records, want expired one pruned", index.count)
	}
	index.expire(testNow.Add(availabilityLease))
	if index.count != 0 || index.bytes != 0 || len(index.keys) != 0 {
		t.Fatalf("expire left count=%d bytes=%d keys=%d", index.count, index.bytes, len(index.keys))
	}
	// Freed slots are reused.
	mustMerge(t, index, testPeerA, testClaim(3, 1, true, testPeerA, testOwner))
	if len(index.slots) != 2 {
		t.Fatalf("slots grew to %d", len(index.slots))
	}
}

func TestAvailabilityIndexSyncLocal(t *testing.T) {
	index := newAvailabilityIndex()
	session := testSession(7)
	lease := testNow.Add(availabilityLease).UnixMilli()
	if dropped := index.syncLocal(testSelf, session, []string{testKey(1), testKey(2), testKey(1), "bad"}, false, testNow); dropped != 1 {
		t.Fatalf("dropped %d, want only invalid key", dropped)
	}
	if got := index.owners(testKey(1), testNow); !slices.Equal(got, []string{testSelf}) {
		t.Fatalf("local owners = %v", got)
	}
	committed := localSnapshot(t, index, testPeerA, testNow)
	for _, claim := range committed {
		if claim.Owner != testSelf || claim.Session != session || !claim.Present || claim.Expires != lease || !slices.Equal(claim.Seen, []string{testSelf}) {
			t.Fatalf("committed claim = %+v", claim)
		}
	}
	if got := index.batch(testSelf, testNow); len(got) != 0 {
		t.Fatalf("local claims offered to self: %+v", got)
	}
	index.ack(testPeerA, index.batch(testPeerA, testNow))

	// Without refresh, unchanged keys keep their revision and seen set.
	later := testNow.Add(time.Minute)
	index.syncLocal(testSelf, session, []string{testKey(1), testKey(2)}, false, later)
	if got := index.batch(testPeerA, later); len(got) != 0 {
		t.Fatalf("unchanged sync created revisions: %+v", got)
	}

	// Refresh renews positives with new revisions, reset seen, and longer leases.
	index.syncLocal(testSelf, session, []string{testKey(1), testKey(2)}, true, later)
	renewed := localSnapshot(t, index, testPeerA, later)
	for key, claim := range renewed {
		if claim.Revision <= committed[key].Revision || claim.Expires != later.Add(availabilityLease).UnixMilli() || !slices.Equal(claim.Seen, []string{testSelf}) {
			t.Fatalf("renewed claim = %+v", claim)
		}
	}
	index.ack(testPeerA, index.batch(testPeerA, later))

	// Removal withdraws without shortening expiry even if the clock moved back.
	earlier := testNow.Add(30 * time.Second)
	index.syncLocal(testSelf, session, []string{testKey(1)}, false, earlier)
	withdrawn := localSnapshot(t, index, testPeerA, earlier)
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawal snapshot = %+v", withdrawn)
	}
	withdrawal := withdrawn[testKey(2)]
	if withdrawal.Present || withdrawal.Revision <= renewed[testKey(2)].Revision || withdrawal.Expires != renewed[testKey(2)].Expires || !slices.Equal(withdrawal.Seen, []string{testSelf}) {
		t.Fatalf("withdrawal = %+v", withdrawal)
	}
	if got := index.owners(testKey(2), earlier); len(got) != 0 {
		t.Fatalf("withdrawn key owners = %v", got)
	}
	index.ack(testPeerA, index.batch(testPeerA, earlier))

	// Refresh leaves unexpired withdrawals unchanged.
	index.syncLocal(testSelf, session, []string{testKey(1)}, true, later)
	if got := localSnapshot(t, index, testPeerA, later); len(got) != 1 || got[testKey(1)].Revision == 0 {
		t.Fatalf("refresh snapshot = %+v", got)
	}
	if entry := index.keys[testKey(2)][claimOwner{testSelf, session}]; fieldsOf(entry.claim) != fieldsOf(withdrawal) {
		t.Fatalf("refresh changed withdrawal: %+v", entry.claim)
	}

	// Re-adding a withdrawn key creates a newer positive.
	index.syncLocal(testSelf, session, []string{testKey(1), testKey(2)}, false, later)
	if got := index.owners(testKey(2), later); !slices.Equal(got, []string{testSelf}) {
		t.Fatalf("re-added owners = %v", got)
	}

	// Expired local records are pruned rather than withdrawn.
	expired := testNow.Add(time.Hour)
	index.syncLocal(testSelf, session, nil, false, expired)
	if index.count != 0 || len(index.local) != 0 {
		t.Fatalf("expired local records left count=%d local=%d", index.count, len(index.local))
	}
}

func TestAvailabilityIndexSyncLocalRejectsInvalidIdentity(t *testing.T) {
	index := newAvailabilityIndex()
	keys := []string{testKey(1), testKey(2)}
	if dropped := index.syncLocal("Self.test:1", testSession(1), keys, false, testNow); dropped != 2 || index.count != 0 {
		t.Fatalf("non-canonical self dropped %d with %d records", dropped, index.count)
	}
	if dropped := index.syncLocal(testSelf, "short", keys, false, testNow); dropped != 2 || index.count != 0 {
		t.Fatalf("invalid session dropped %d with %d records", dropped, index.count)
	}
}

func TestAvailabilityIndexConcurrentUpdatesAndReaders(t *testing.T) {
	index := newAvailabilityIndex()
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for revision := uint64(1); revision <= 50; revision++ {
				switch worker {
				case 0:
					claim := testClaim(1, revision, revision < 50, testPeerA, testOwner)
					if _, err := index.merge([]Claim{claim}, testSelf, testPeerA, testNow); err != nil {
						t.Error(err)
					}
				case 1:
					index.syncLocal(testSelf, testSession(2), []string{testKey(2)}, true, testNow)
				case 2:
					index.ack(testPeerB, index.batch(testPeerB, testNow))
				case 3:
					index.owners(testKey(1), testNow)
					index.expire(testNow)
				}
			}
		})
	}
	workers.Wait()
	if got := index.owners(testKey(1), testNow); len(got) != 0 {
		t.Fatalf("final withdrawal lost during concurrent access: %v", got)
	}
	if got := index.owners(testKey(2), testNow); !slices.Equal(got, []string{testSelf}) {
		t.Fatalf("local publication lost during concurrent access: %v", got)
	}
}

// localSnapshot returns the claims offered to destination, keyed by key.
func localSnapshot(t *testing.T, index *availabilityIndex, destination string, now time.Time) map[string]Claim {
	t.Helper()
	claims := map[string]Claim{}
	for _, claim := range index.batch(destination, now) {
		claims[claim.Key] = claim
	}
	return claims
}

func mustMerge(t *testing.T, index *availabilityIndex, sender string, claims ...Claim) int {
	t.Helper()
	dropped, err := index.merge(claims, testSelf, sender, testNow)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	return dropped
}

// testClaim returns a claim from testOwner's first session expiring after a
// full lease. Seen must already be sorted.
func testClaim(key int, revision uint64, present bool, seen ...string) Claim {
	return Claim{
		Owner:    testOwner,
		Session:  testSession(1),
		Key:      testKey(key),
		Revision: revision,
		Present:  present,
		Expires:  testNow.Add(availabilityLease).UnixMilli(),
		Seen:     slices.Clone(seen),
	}
}

// testAddresses returns n sorted canonical addresses including required.
func testAddresses(n int, required ...string) []string {
	addresses := slices.Clone(required)
	for i := 0; len(addresses) < n; i++ {
		addresses = append(addresses, fmt.Sprintf("host-%04d.example.test:1", i))
	}
	slices.Sort(addresses)
	return addresses
}

func testSession(i int) string { return fmt.Sprintf("%032x", i) }

func claimEqual(a, b Claim) bool {
	return fieldsOf(a) == fieldsOf(b) && slices.Equal(a.Seen, b.Seen)
}
