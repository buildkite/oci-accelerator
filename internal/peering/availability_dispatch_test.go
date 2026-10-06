package peering

import (
	"slices"
	"testing"
	"time"
)

func TestAvailabilityRelayPreservesOwnerAndAccumulatesSeen(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E")
	// A chain: each node knows only its neighbours.
	f.know("A", "B")
	f.know("B", "A", "C")
	f.know("C", "B", "D")
	f.know("D", "C", "E")
	f.know("E", "D")
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()

	// Each node has seen the path so far plus the next hop it reached.
	names := []string{"A", "B", "C", "D", "E"}
	for i, name := range names[1:] {
		want := names[:min(i+3, len(names))]
		h := f.hint(name, "A", key)
		if h == nil {
			t.Fatalf("%s has no hint", name)
		}
		if got := f.seenNames(h); !slices.Equal(got, want) {
			t.Fatalf("%s seen = %v, want %v", name, got, want)
		}
		if h.pending {
			t.Fatalf("%s pass still pending", name)
		}
	}
	// E knows only D, which is seen, so it sends nothing.
	if got := len(f.requests("E")); got != 0 {
		t.Fatalf("E sent %d requests", got)
	}
	if got := len(f.sent); got != 4 {
		t.Fatalf("requests = %d, want one per hop", got)
	}
}

func TestAvailabilityPassStopsAfterTwoAcknowledgements(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E")
	f.know("A", "B", "C", "D", "E")
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()

	sent := f.requests("A")
	if len(sent) != hintFanout {
		t.Fatalf("A sent %d requests, want %d", len(sent), hintFanout)
	}
	h := f.hint("A", "A", key)
	if h.pending || h.ackCount != hintFanout {
		t.Fatalf("pending = %v, acknowledged = %d", h.pending, h.ackCount)
	}
	// Two peers remain unseen, and that is accepted.
	if got := f.seenNames(h); len(got) != 1+hintFanout {
		t.Fatalf("seen = %v", got)
	}
}

func TestAvailabilityDuplicateDoesNotReopenCompletedPass(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.know("B", "A", "C")
	key := testCacheKey(1)
	f.originate("C", key)
	claim := testClaim(t, f.byName["C"].id, key, f.byName["C"].id)
	// C knows nobody, so deliver its claim to A directly.
	if err := f.byName["A"].a.accept(availabilityMessage{sender: claim.owner, recipient: mustNodeID(t, f.byName["A"].id), claims: []hintClaim{claim}}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	h := f.hint("A", "C", key)
	if h == nil || h.pending {
		t.Fatalf("A hint = %+v", h)
	}
	before := len(f.sent)

	// A duplicate with new seen IDs merges without restarting forwarding.
	duplicate := testClaim(t, f.byName["C"].id, key, f.byName["C"].id, testNodeID(9))
	if err := f.byName["A"].a.accept(availabilityMessage{sender: claim.owner, recipient: mustNodeID(t, f.byName["A"].id), claims: []hintClaim{duplicate}}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if len(f.sent) != before || h.pending || !containsNodeID(h.seen, mustNodeID(t, testNodeID(9))) {
		t.Fatalf("duplicate reopened the pass: sent %d more, pending %v", len(f.sent)-before, h.pending)
	}
}

func TestAvailabilityPartialBooksLeaveNodesUninformed(t *testing.T) {
	// A knows B, C, D. B and C know A and each other. D knows A and E, and E
	// knows only D. Find the seed for which A chooses B and C.
	for seed := uint64(1); seed < 200; seed++ {
		f := newTestFleet(t, seed, "A", "B", "C", "D", "E")
		f.know("A", "B", "C", "D")
		f.know("B", "A", "C")
		f.know("C", "A", "B")
		f.know("D", "A", "E")
		f.know("E", "D")
		key := testCacheKey(1)
		f.originate("A", key)
		f.step()
		first := f.requests("A")
		if len(first) != 2 || first[0].to == "D" || first[1].to == "D" {
			continue
		}
		f.settle()
		if f.hint("D", "A", key) != nil || f.hint("E", "A", key) != nil {
			t.Fatal("D or E received the hint through a hidden fallback")
		}
		// Discovery learning another peer later does not replay the hint.
		f.know("B", "E")
		f.know("A", "E")
		f.settle()
		if f.hint("E", "A", key) != nil {
			t.Fatal("a newly discovered peer received a completed hint")
		}
		return
	}
	t.Fatal("no seed made A choose B and C")
}

func TestAvailabilityAllBookSeenFinishesWithoutSending(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	f.know("B", "A")
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()
	if got := len(f.requests("B")); got != 0 {
		t.Fatalf("B sent %d requests, though A already has the hint", got)
	}
	if h := f.hint("B", "A", key); h == nil || h.pending {
		t.Fatalf("B hint = %+v", h)
	}
	// A finished with one acknowledgement because nothing else is unseen.
	if h := f.hint("A", "A", key); h.pending || h.ackCount != 1 {
		t.Fatalf("A pending = %v, acknowledged = %d", h.pending, h.ackCount)
	}
}

func TestAvailabilityDuplicateBeforeDispatchRemovesSeenDestinations(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "O")
	f.know("A", "B", "C")
	key := testCacheKey(1)
	owner, b := f.byName["O"].id, f.byName["B"].id
	self := mustNodeID(t, f.byName["A"].id)
	first := testClaim(t, owner, key, owner)
	if err := f.byName["A"].a.accept(availabilityMessage{sender: first.owner, recipient: self, claims: []hintClaim{first}}); err != nil {
		t.Fatal(err)
	}
	// Before A dispatches, a duplicate reports that B already has the hint.
	duplicate := testClaim(t, owner, key, owner, b)
	if err := f.byName["A"].a.accept(availabilityMessage{sender: mustNodeID(t, b), recipient: self, claims: []hintClaim{duplicate}}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	sent := f.requests("A")
	if len(sent) != 1 || sent[0].to != "C" {
		t.Fatalf("A sent %+v, want only C", sent)
	}
	if h := f.hint("A", "O", key); h.pending {
		t.Fatal("pass still pending with every resolved peer seen")
	}
}

func TestAvailabilityFailuresConsumeAttemptBudget(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	f.fault = func(fleetRequest) (bool, bool) { return false, true }
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()
	if got := len(f.requests("A")); got != hintAttempts {
		t.Fatalf("A sent %d requests, want %d", got, hintAttempts)
	}
	h := f.hint("A", "A", key)
	if h.pending || h.attempts != 0 || h.ackCount != 0 || len(h.seen) != 1 {
		t.Fatalf("pending %v attempts %d acknowledged %d seen %d", h.pending, h.attempts, h.ackCount, len(h.seen))
	}
	if stats := f.byName["A"].a.Stats(); stats.Failed != hintAttempts || stats.Acknowledged != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAvailabilityLostAcknowledgementsStayWithinBudgets(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	// Requests arrive, but every response is lost.
	f.fault = func(r fleetRequest) (bool, bool) { return true, r.from == "A" }
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()
	sent := f.requests("A")
	if len(sent) != hintAttempts {
		t.Fatalf("A sent %d requests, want %d", len(sent), hintAttempts)
	}
	// Ambiguous failures can reach more than two nodes, and repeat deliveries
	// merge as duplicates.
	for _, name := range []string{"B", "C", "D"} {
		if f.hint(name, "A", key) == nil {
			t.Fatalf("%s did not receive the hint", name)
		}
	}
	if h := f.hint("A", "A", key); h.ackCount != 0 || len(h.seen) != 1 {
		t.Fatal("lost responses were credited as acknowledgements")
	}
}

func TestAvailabilityReplacementAfterFailureReachesFanout(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	f.fault = func(r fleetRequest) (bool, bool) { return r.to != "B", r.to == "B" }
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()
	h := f.hint("A", "A", key)
	if h.ackCount != hintFanout || !slices.Equal(f.seenNames(h), []string{"A", "C", "D"}) {
		t.Fatalf("acknowledged %d, seen %v", h.ackCount, f.seenNames(h))
	}
	if sent := len(f.requests("A")); sent > hintAttempts {
		t.Fatalf("A sent %d requests", sent)
	}
}

func TestAvailabilityPendingPassSelectsNewlyDiscoveredPeer(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B")
	f.fault = func(r fleetRequest) (bool, bool) { return r.to != "B", r.to == "B" }
	key := testCacheKey(1)
	f.originate("A", key)
	f.step()
	h := f.hint("A", "A", key)
	if !h.pending {
		t.Fatal("pass finished despite an unseen peer in backoff")
	}
	// Discovery learns C while the pass is still pending.
	f.know("A", "C")
	f.settle()
	if f.hint("C", "A", key) == nil || !containsNodeID(h.seen, mustNodeID(t, f.byName["C"].id)) {
		t.Fatal("pending pass did not select the newly discovered peer")
	}
}

func TestAvailabilityDispatchWaitsWhileEveryDestinationIsBusy(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	f.originate("A", testCacheKey(1))
	f.now = f.now.Add(2 * availabilityCadence)
	first := a.dispatch(availabilityConcurrency)
	f.originate("A", testCacheKey(2))
	f.now = f.now.Add(2 * availabilityCadence)
	if more := a.dispatch(availabilityConcurrency); len(more) != 0 {
		t.Fatalf("busy destination received %d requests", len(more))
	}
	// The due hint stays queued, and Run sleeps until a result instead of
	// spinning on it.
	if h := f.hint("A", "A", testCacheKey(2)); h.queued < 0 {
		t.Fatal("due hint was dequeued while no destination was idle")
	}
	if wait := a.nextWait(1); wait < time.Minute {
		t.Fatalf("nextWait = %v while every destination is busy", wait)
	}
	a.complete(first[0], nil)
	if got := a.dispatch(availabilityConcurrency); len(got) != 1 {
		t.Fatalf("no dispatch after the destination freed: %d", len(got))
	}
}

func TestAvailabilityDispatchReservesWithinRemainingFanout(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E", "F")
	f.know("A", "B", "C", "D", "E", "F")
	a := f.byName["A"].a
	key := testCacheKey(1)
	f.originate("A", key)
	f.now = f.now.Add(2 * availabilityCadence)
	first := a.dispatch(availabilityConcurrency)
	if len(first) != hintFanout {
		t.Fatalf("first dispatch created %d requests", len(first))
	}
	if again := a.dispatch(availabilityConcurrency); len(again) != 0 {
		t.Fatalf("in-flight work allowed %d more requests", len(again))
	}
	h := f.hint("A", "A", key)
	if h.inflight != hintFanout || h.attempts != hintAttempts-hintFanout {
		t.Fatalf("inflight %d attempts %d", h.inflight, h.attempts)
	}
	// One failure frees one slot for one replacement.
	a.complete(first[0], errAvailabilitySend)
	a.complete(first[1], nil)
	f.now = f.now.Add(2 * availabilityCadence)
	replacement := a.dispatch(availabilityConcurrency)
	if len(replacement) != 1 || replacement[0].to.id == first[0].to.id || replacement[0].to.id == first[1].to.id {
		t.Fatalf("replacement = %+v", replacement)
	}
}

func TestAvailabilityBatchesClaimsPerDestinationWithinBounds(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	keys := make([]string, maxAvailabilityClaim+88)
	for i := range keys {
		keys[i] = testCacheKey(i).String()
	}
	f.byName["A"].inventory.set(keys...)
	a.reconcile(f.byName["A"].inventory.Keys())
	f.now = f.now.Add(2 * availabilityCadence)
	batches := a.dispatch(availabilityConcurrency)
	if len(batches) != 1 || len(batches[0].message.claims) != maxAvailabilityClaim {
		t.Fatalf("batches = %d, claims = %d", len(batches), len(batches[0].message.claims))
	}
	encoded, err := encodeAvailability(batches[0].message)
	if err != nil || len(encoded) != batches[0].size {
		t.Fatalf("encoded %d bytes, accounted %d, err %v", len(encoded), batches[0].size, err)
	}
	// One destination allows one request at a time.
	if more := a.dispatch(availabilityConcurrency); len(more) != 0 {
		t.Fatalf("busy destination received %d more requests", len(more))
	}
	a.complete(batches[0], nil)
	f.now = f.now.Add(2 * availabilityCadence)
	rest := a.dispatch(availabilityConcurrency)
	if len(rest) != 1 || len(rest[0].message.claims) != 88 {
		t.Fatalf("remaining batches = %+v", len(rest))
	}
}

func TestAvailabilityBatchSizeBoundSplitsLargeClaims(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	// Remote hints with full seen sets are about 15 KiB each on the wire.
	sender := mustNodeID(t, testNodeID(2000))
	for i := range 40 {
		ids := []string{testNodeID(2000)}
		for j := range maxHintSeen - 2 {
			ids = append(ids, testNodeID(3000+j))
		}
		claim := testClaim(t, testNodeID(2000), testCacheKey(i), ids...)
		if err := a.accept(availabilityMessage{sender: sender, recipient: a.self, claims: []hintClaim{claim}}); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(2 * availabilityCadence)
	batches := a.dispatch(availabilityConcurrency)
	if len(batches) != 1 || len(batches[0].message.claims) >= 40 {
		t.Fatalf("batch claims = %d", len(batches[0].message.claims))
	}
	encoded, err := encodeAvailability(batches[0].message)
	if err != nil || len(encoded) != batches[0].size || len(encoded) > maxAvailabilityBytes {
		t.Fatalf("encoded %d bytes, accounted %d, err %v", len(encoded), batches[0].size, err)
	}
}

func TestAvailabilitySameIDAddressMoveKeepsSeenAndBudget(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.fault = func(fleetRequest) (bool, bool) { return false, true }
	key := testCacheKey(1)
	f.originate("A", key)
	f.step()
	h := f.hint("A", "A", key)
	attempts := h.attempts
	// B moves to a new address. Backoff for the old pairing is dropped, but
	// the hint keeps its attempt budget.
	b := f.byName["B"]
	delete(f.byAddress, b.address)
	b.address = "b-moved.test:443"
	f.byAddress[b.address] = b
	f.bind("A", b.address, b.id)
	f.fault = nil
	f.settle()
	if h.ackCount != hintFanout || h.attempts >= attempts {
		t.Fatalf("acknowledged %d attempts %d (before %d)", h.ackCount, h.attempts, attempts)
	}
	last := f.requests("A")
	if r := last[len(last)-1]; r.to != "B" && r.to != "C" {
		t.Fatalf("last request went to %s", r.to)
	}
	if !slices.Equal(f.seenNames(h), []string{"A", "B", "C"}) {
		t.Fatalf("seen = %v", f.seenNames(h))
	}
	if f.hint("B", "A", key) == nil {
		t.Fatal("B did not receive the hint at its new address")
	}
}

func TestAvailabilityAddressReuseByAnotherNodeIsNotCredited(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	// A's book still maps C's address to B, as after an address reuse.
	f.bind("A", f.byName["C"].address, f.byName["B"].id)
	key := testCacheKey(1)
	f.originate("A", key)
	f.settle()
	sent := f.requests("A")
	if len(sent) == 0 || sent[0].to != "C" || sent[0].recipient != f.byName["B"].id {
		t.Fatalf("A sent %+v", sent)
	}
	if f.hint("C", "A", key) != nil {
		t.Fatal("C merged a request addressed to B")
	}
	h := f.hint("A", "A", key)
	if h.ackCount != 0 || len(h.seen) != 1 {
		t.Fatal("a wrong-recipient rejection was credited")
	}
	// Availability never changes discovery's mappings.
	if peers := f.byName["A"].a.book.Snapshot().Peers; len(peers) != 1 || peers[f.byName["C"].address] != f.byName["B"].id {
		t.Fatalf("book = %v", peers)
	}
}

func TestAvailabilityStaleResultDoesNotTouchReplacementRecord(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	a := f.byName["A"].a
	key := testCacheKey(1)
	f.originate("A", key)
	f.now = f.now.Add(2 * availabilityCadence)
	batches := a.dispatch(availabilityConcurrency)
	if len(batches) != hintFanout {
		t.Fatalf("batches = %d", len(batches))
	}
	// The entry is removed and recommitted while requests are in flight.
	f.byName["A"].inventory.set()
	a.reconcile(nil)
	f.originate("A", key)
	replacement := f.hint("A", "A", key)
	for _, batch := range batches {
		a.complete(batch, nil)
	}
	if replacement.ackCount != 0 || replacement.inflight != 0 || replacement.attempts != hintAttempts || len(replacement.seen) != 1 {
		t.Fatalf("stale results changed the replacement: %+v", replacement)
	}
	if !replacement.pending {
		t.Fatal("replacement pass finished early")
	}
}

func TestAvailabilityEvictedRecordIsNotRecreatedByAcknowledgement(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	a.index = newBoundedHintIndex(a.self, hintLimits{records: 1, cost: maxHintCost, encoded: hintSnapshotBudget})
	f.originate("A", testCacheKey(1))
	f.now = f.now.Add(2 * availabilityCadence)
	batches := a.dispatch(availabilityConcurrency)
	f.originate("A", testCacheKey(2)) // evicts the first record
	a.complete(batches[0], nil)
	if f.hint("A", "A", testCacheKey(1)) != nil || a.index.len() != 1 {
		t.Fatal("acknowledgement recreated an evicted record")
	}
}

func TestAvailabilityUnknownOwnersStayAdvisory(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	before := a.book.Snapshot()
	unknown := testNodeID(77)
	claim := testClaim(t, unknown, testCacheKey(1), unknown, f.byName["B"].id)
	if err := a.accept(availabilityMessage{sender: mustNodeID(t, f.byName["B"].id), recipient: a.self, claims: []hintClaim{claim}}); err != nil {
		t.Fatal(err)
	}
	if got := a.Owners(testCacheKey(1).String()); !slices.Equal(got, []string{unknown}) {
		t.Fatalf("owners = %v", got)
	}
	if after := a.book.Snapshot(); len(after.Peers) != len(before.Peers) {
		t.Fatalf("availability changed discovery: %v", after.Peers)
	}
	f.settle()
	if sent := f.requests("A"); len(sent) != 0 {
		t.Fatalf("A relayed to seen peers: %+v", sent)
	}
}

func TestAvailabilityIdleNodesSendNothing(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.know("B", "A", "C")
	f.know("C", "A", "B")
	f.originate("A", testCacheKey(1))
	f.settle()
	before := len(f.sent)
	for range 50 {
		f.step()
	}
	if len(f.sent) != before {
		t.Fatalf("idle fleet sent %d refreshes", len(f.sent)-before)
	}
}

func TestAvailabilityBusyDestinationDoesNotBlockOthers(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	a := f.byName["A"].a
	f.originate("A", testCacheKey(1))
	f.now = f.now.Add(2 * availabilityCadence)
	first := a.dispatch(availabilityConcurrency)
	busy := map[nodeID]bool{}
	for _, batch := range first {
		busy[batch.to.id] = true
	}
	// Another hint becomes due while both requests are still in flight.
	f.originate("A", testCacheKey(2))
	f.now = f.now.Add(2 * availabilityCadence)
	second := a.dispatch(availabilityConcurrency)
	if len(second) != 1 || busy[second[0].to.id] {
		t.Fatalf("second dispatch = %+v", second)
	}
}

func TestClaimWireSizeMatchesEncoding(t *testing.T) {
	sender, recipient := mustNodeID(t, testIDB), mustNodeID(t, testIDC)
	for _, n := range []int{1, 2, 3, maxHintSeen} {
		ids := []string{testIDB}
		for i := range n - 1 {
			ids = append(ids, testNodeID(i))
		}
		claim := testClaim(t, testIDB, testCacheKey(n), ids...)
		one, err := encodeAvailability(availabilityMessage{sender: sender, recipient: recipient, claims: []hintClaim{claim}})
		if err != nil {
			t.Fatal(err)
		}
		if want := availabilityEnvelopeSize + claimWireSize(n); len(one) != want {
			t.Fatalf("seen %d: encoded %d, computed %d", n, len(one), want)
		}
	}
}

func TestAvailabilityBackoffDelaysRetriesToFailedDestination(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	f.originate("A", testCacheKey(1))
	f.now = f.now.Add(2 * availabilityCadence)
	first := a.dispatch(availabilityConcurrency)
	a.complete(first[0], errAvailabilitySend)
	f.now = f.now.Add(2 * availabilityCadence)
	second := a.dispatch(availabilityConcurrency)
	if len(second) != 1 {
		t.Fatalf("no retry after the first backoff: %d", len(second))
	}
	// A second failure doubles the delay to about two seconds.
	a.complete(second[0], errAvailabilitySend)
	f.now = f.now.Add(1500 * time.Millisecond)
	if got := a.dispatch(availabilityConcurrency); len(got) != 0 {
		t.Fatal("retried a destination during its backoff")
	}
	f.now = f.now.Add(availabilityCadence)
	if got := a.dispatch(availabilityConcurrency); len(got) != 1 {
		t.Fatalf("no retry after the second backoff: %d", len(got))
	}
}
