package peering

import (
	"slices"
	"testing"
	"time"
)

func TestProviderHintsRelayPreservesProviderAndAccumulatesSeen(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E")
	// A chain: each node knows only its neighbours.
	f.know("A", "B")
	f.know("B", "A", "C")
	f.know("C", "B", "D")
	f.know("D", "C", "E")
	f.know("E", "D")
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsPassStopsAfterTwoAcknowledgements(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E")
	f.know("A", "B", "C", "D", "E")
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsDuplicateDoesNotReopenCompletedPass(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.know("B", "A", "C")
	key := testCacheKey(1)
	f.announce("C", key)
	announced := testHint(t, f.byName["C"].id, key, f.byName["C"].id)
	// C knows nobody, so deliver its hint to A directly.
	if err := f.byName["A"].a.accept(announcement{sender: announced.provider, recipient: mustNodeID(t, f.byName["A"].id), hints: []announcedHint{announced}}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	h := f.hint("A", "C", key)
	if h == nil || h.pending {
		t.Fatalf("A hint = %+v", h)
	}
	before := len(f.sent)

	// A duplicate with new seen IDs merges without restarting forwarding.
	duplicate := testHint(t, f.byName["C"].id, key, f.byName["C"].id, testNodeID(9))
	if err := f.byName["A"].a.accept(announcement{sender: announced.provider, recipient: mustNodeID(t, f.byName["A"].id), hints: []announcedHint{duplicate}}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if len(f.sent) != before || h.pending || !containsNodeID(h.seen, mustNodeID(t, testNodeID(9))) {
		t.Fatalf("duplicate reopened the pass: sent %d more, pending %v", len(f.sent)-before, h.pending)
	}
}

func TestProviderHintsPartialBooksLeaveNodesUninformed(t *testing.T) {
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
		f.announce("A", key)
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

func TestProviderHintsAllBookSeenFinishesWithoutSending(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	f.know("B", "A")
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsDuplicateBeforeDispatchRemovesSeenDestinations(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "O")
	f.know("A", "B", "C")
	key := testCacheKey(1)
	provider, b := f.byName["O"].id, f.byName["B"].id
	self := mustNodeID(t, f.byName["A"].id)
	first := testHint(t, provider, key, provider)
	if err := f.byName["A"].a.accept(announcement{sender: first.provider, recipient: self, hints: []announcedHint{first}}); err != nil {
		t.Fatal(err)
	}
	// Before A dispatches, a duplicate reports that B already has the hint.
	duplicate := testHint(t, provider, key, provider, b)
	if err := f.byName["A"].a.accept(announcement{sender: mustNodeID(t, b), recipient: self, hints: []announcedHint{duplicate}}); err != nil {
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

func TestProviderHintsFailuresConsumeAttemptBudget(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	f.fault = func(fleetRequest) (bool, bool) { return false, true }
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsLostAcknowledgementsStayWithinBudgets(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	// Requests arrive, but every response is lost.
	f.fault = func(r fleetRequest) (bool, bool) { return true, r.from == "A" }
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsReplacementAfterFailureReachesFanout(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	f.fault = func(r fleetRequest) (bool, bool) { return r.to != "B", r.to == "B" }
	key := testCacheKey(1)
	f.announce("A", key)
	f.settle()
	h := f.hint("A", "A", key)
	if h.ackCount != hintFanout || !slices.Equal(f.seenNames(h), []string{"A", "C", "D"}) {
		t.Fatalf("acknowledged %d, seen %v", h.ackCount, f.seenNames(h))
	}
	if sent := len(f.requests("A")); sent > hintAttempts {
		t.Fatalf("A sent %d requests", sent)
	}
}

func TestProviderHintsPendingPassSelectsNewlyDiscoveredPeer(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B")
	f.fault = func(r fleetRequest) (bool, bool) { return r.to != "B", r.to == "B" }
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsDispatchWaitsWhileEveryDestinationIsBusy(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	f.announce("A", testCacheKey(1))
	f.now = f.now.Add(2 * announceCadence)
	first := a.dispatch(announceConcurrency)
	f.announce("A", testCacheKey(2))
	f.now = f.now.Add(2 * announceCadence)
	if more := a.dispatch(announceConcurrency); len(more) != 0 {
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
	if got := a.dispatch(announceConcurrency); len(got) != 1 {
		t.Fatalf("no dispatch after the destination freed: %d", len(got))
	}
}

func TestProviderHintsDispatchReservesWithinRemainingFanout(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D", "E", "F")
	f.know("A", "B", "C", "D", "E", "F")
	a := f.byName["A"].a
	key := testCacheKey(1)
	f.announce("A", key)
	f.now = f.now.Add(2 * announceCadence)
	first := a.dispatch(announceConcurrency)
	if len(first) != hintFanout {
		t.Fatalf("first dispatch created %d requests", len(first))
	}
	if again := a.dispatch(announceConcurrency); len(again) != 0 {
		t.Fatalf("in-flight work allowed %d more requests", len(again))
	}
	h := f.hint("A", "A", key)
	if h.inflight != hintFanout || h.attempts != hintAttempts-hintFanout {
		t.Fatalf("inflight %d attempts %d", h.inflight, h.attempts)
	}
	// One failure frees one slot for one replacement.
	a.complete(first[0], errAnnounceSend)
	a.complete(first[1], nil)
	f.now = f.now.Add(2 * announceCadence)
	replacement := a.dispatch(announceConcurrency)
	if len(replacement) != 1 || replacement[0].to.id == first[0].to.id || replacement[0].to.id == first[1].to.id {
		t.Fatalf("replacement = %+v", replacement)
	}
}

func TestProviderHintsBatchesHintsPerDestinationWithinBounds(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	keys := make([]string, maxAnnouncementHints+88)
	for i := range keys {
		keys[i] = testCacheKey(i).String()
	}
	f.byName["A"].inventory.set(keys...)
	a.reconcile(f.byName["A"].inventory.Keys())
	f.now = f.now.Add(2 * announceCadence)
	batches := a.dispatch(announceConcurrency)
	if len(batches) != 1 || len(batches[0].message.hints) != maxAnnouncementHints {
		t.Fatalf("batches = %d, hints = %d", len(batches), len(batches[0].message.hints))
	}
	encoded, err := encodeAnnouncement(batches[0].message)
	if err != nil || len(encoded) != batches[0].size {
		t.Fatalf("encoded %d bytes, accounted %d, err %v", len(encoded), batches[0].size, err)
	}
	// One destination allows one request at a time.
	if more := a.dispatch(announceConcurrency); len(more) != 0 {
		t.Fatalf("busy destination received %d more requests", len(more))
	}
	a.complete(batches[0], nil)
	f.now = f.now.Add(2 * announceCadence)
	rest := a.dispatch(announceConcurrency)
	if len(rest) != 1 || len(rest[0].message.hints) != 88 {
		t.Fatalf("remaining batches = %+v", len(rest))
	}
}

func TestProviderHintsBatchSizeBoundSplitsLargeHints(t *testing.T) {
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
		announced := testHint(t, testNodeID(2000), testCacheKey(i), ids...)
		if err := a.accept(announcement{sender: sender, recipient: a.self, hints: []announcedHint{announced}}); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(2 * announceCadence)
	batches := a.dispatch(announceConcurrency)
	if len(batches) != 1 || len(batches[0].message.hints) >= 40 {
		t.Fatalf("batch hints = %d", len(batches[0].message.hints))
	}
	encoded, err := encodeAnnouncement(batches[0].message)
	if err != nil || len(encoded) != batches[0].size || len(encoded) > maxAnnouncementBytes {
		t.Fatalf("encoded %d bytes, accounted %d, err %v", len(encoded), batches[0].size, err)
	}
}

func TestProviderHintsSameIDAddressMoveKeepsSeenAndBudget(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.fault = func(fleetRequest) (bool, bool) { return false, true }
	key := testCacheKey(1)
	f.announce("A", key)
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

func TestProviderHintsAddressReuseByAnotherNodeIsNotCredited(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	// A's book still maps C's address to B, as after an address reuse.
	f.bind("A", f.byName["C"].address, f.byName["B"].id)
	key := testCacheKey(1)
	f.announce("A", key)
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
	// Provider hints never change discovery's mappings.
	if peers := f.byName["A"].a.book.Snapshot().Peers; len(peers) != 1 || peers[f.byName["C"].address] != f.byName["B"].id {
		t.Fatalf("book = %v", peers)
	}
}

func TestProviderHintsStaleResultDoesNotTouchReplacementRecord(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	a := f.byName["A"].a
	key := testCacheKey(1)
	f.announce("A", key)
	f.now = f.now.Add(2 * announceCadence)
	batches := a.dispatch(announceConcurrency)
	if len(batches) != hintFanout {
		t.Fatalf("batches = %d", len(batches))
	}
	// The entry is removed and recommitted while requests are in flight.
	f.byName["A"].inventory.set()
	a.reconcile(nil)
	f.announce("A", key)
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

func TestProviderHintsEvictedRecordIsNotRecreatedByAcknowledgement(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	a.index = newBoundedHintIndex(a.self, hintLimits{records: 1, cost: maxHintCost, encoded: hintSnapshotBudget})
	f.announce("A", testCacheKey(1))
	f.now = f.now.Add(2 * announceCadence)
	batches := a.dispatch(announceConcurrency)
	f.announce("A", testCacheKey(2)) // evicts the first record
	a.complete(batches[0], nil)
	if f.hint("A", "A", testCacheKey(1)) != nil || a.index.len() != 1 {
		t.Fatal("acknowledgement recreated an evicted record")
	}
}

func TestProviderHintsUnknownProvidersStayAdvisory(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	before := a.book.Snapshot()
	unknown := testNodeID(77)
	announced := testHint(t, unknown, testCacheKey(1), unknown, f.byName["B"].id)
	if err := a.accept(announcement{sender: mustNodeID(t, f.byName["B"].id), recipient: a.self, hints: []announcedHint{announced}}); err != nil {
		t.Fatal(err)
	}
	if got := a.Providers(testCacheKey(1).String()); !slices.Equal(got, []string{unknown}) {
		t.Fatalf("providers = %v", got)
	}
	if after := a.book.Snapshot(); len(after.Peers) != len(before.Peers) {
		t.Fatalf("provider hints changed discovery: %v", after.Peers)
	}
	f.settle()
	if sent := f.requests("A"); len(sent) != 0 {
		t.Fatalf("A relayed to seen peers: %+v", sent)
	}
}

func TestProviderHintsIdleNodesSendNothing(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B", "C")
	f.know("B", "A", "C")
	f.know("C", "A", "B")
	f.announce("A", testCacheKey(1))
	f.settle()
	before := len(f.sent)
	for range 50 {
		f.step()
	}
	if len(f.sent) != before {
		t.Fatalf("idle fleet sent %d refreshes", len(f.sent)-before)
	}
}

func TestProviderHintsBusyDestinationDoesNotBlockOthers(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C", "D")
	f.know("A", "B", "C", "D")
	a := f.byName["A"].a
	f.announce("A", testCacheKey(1))
	f.now = f.now.Add(2 * announceCadence)
	first := a.dispatch(announceConcurrency)
	busy := map[nodeID]bool{}
	for _, batch := range first {
		busy[batch.to.id] = true
	}
	// Another hint becomes due while both requests are still in flight.
	f.announce("A", testCacheKey(2))
	f.now = f.now.Add(2 * announceCadence)
	second := a.dispatch(announceConcurrency)
	if len(second) != 1 || busy[second[0].to.id] {
		t.Fatalf("second dispatch = %+v", second)
	}
}

func TestHintWireSizeMatchesEncoding(t *testing.T) {
	sender, recipient := mustNodeID(t, testIDB), mustNodeID(t, testIDC)
	for _, n := range []int{1, 2, 3, maxHintSeen} {
		ids := []string{testIDB}
		for i := range n - 1 {
			ids = append(ids, testNodeID(i))
		}
		announced := testHint(t, testIDB, testCacheKey(n), ids...)
		one, err := encodeAnnouncement(announcement{sender: sender, recipient: recipient, hints: []announcedHint{announced}})
		if err != nil {
			t.Fatal(err)
		}
		if want := announcementEnvelopeSize + hintWireSize(n); len(one) != want {
			t.Fatalf("seen %d: encoded %d, computed %d", n, len(one), want)
		}
	}
}

func TestProviderHintsBackoffDelaysRetriesToFailedDestination(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	f.announce("A", testCacheKey(1))
	f.now = f.now.Add(2 * announceCadence)
	first := a.dispatch(announceConcurrency)
	a.complete(first[0], errAnnounceSend)
	f.now = f.now.Add(2 * announceCadence)
	second := a.dispatch(announceConcurrency)
	if len(second) != 1 {
		t.Fatalf("no retry after the first backoff: %d", len(second))
	}
	// A second failure doubles the delay to about two seconds.
	a.complete(second[0], errAnnounceSend)
	f.now = f.now.Add(1500 * time.Millisecond)
	if got := a.dispatch(announceConcurrency); len(got) != 0 {
		t.Fatal("retried a destination during its backoff")
	}
	f.now = f.now.Add(announceCadence)
	if got := a.dispatch(announceConcurrency); len(got) != 1 {
		t.Fatalf("no retry after the second backoff: %d", len(got))
	}
}
