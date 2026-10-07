package peering

import (
	"slices"
	"testing"
)

func TestProviderHintsEvictionSendsNoWithdrawal(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	f.know("B", "A")
	key, other := testCacheKey(1), testCacheKey(2)
	f.announce("A", key)
	f.announce("A", other)
	f.settle()
	before := len(f.sent)

	f.byName["A"].inventory.set(other.String())
	f.byName["A"].a.reconcile(f.byName["A"].inventory.Keys())
	f.settle()
	if len(f.sent) != before {
		t.Fatalf("eviction sent %d requests", len(f.sent)-before)
	}
	if f.hint("A", "A", key) != nil {
		t.Fatal("A kept its hint for an evicted key")
	}
	if f.hint("A", "A", other) == nil {
		t.Fatal("A forgot an unrelated key")
	}
	if f.hint("B", "A", key) == nil {
		t.Fatal("B's remote hint was withdrawn")
	}
}

func TestProviderHintsRemovalCancelsPendingLocalWork(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	key := testCacheKey(1)
	f.announce("A", key)
	f.byName["A"].inventory.set()
	f.byName["A"].a.reconcile(nil)
	f.settle()
	if len(f.sent) != 0 {
		t.Fatalf("removed key was still sent: %+v", f.sent)
	}
}

func TestProviderHintsRecommitDoesNotResetRemoteForwarding(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B", "C")
	f.know("A", "B")
	f.know("B", "A", "C")
	key := testCacheKey(1)
	f.announce("A", key)
	f.settle()
	remote := f.hint("B", "A", key)
	if remote == nil || remote.pending {
		t.Fatalf("B hint = %+v", remote)
	}
	fromB := len(f.requests("B"))

	// Removal and recommit start a fresh local pass on A.
	f.byName["A"].inventory.set()
	f.byName["A"].a.reconcile(nil)
	f.announce("A", key)
	f.settle()
	if got := len(f.requests("A")); got != 2 {
		t.Fatalf("A sent %d requests, want one per pass", got)
	}
	if f.hint("B", "A", key) != remote || remote.pending || len(f.requests("B")) != fromB {
		t.Fatal("recommit restarted B's completed forwarding")
	}
}

func TestProviderHintsUnrelatedChangesDoNotReannounceForgottenHints(t *testing.T) {
	f := newTestFleet(t, 1, "A", "B")
	f.know("A", "B")
	a := f.byName["A"].a
	a.index = newBoundedHintIndex(a.self, hintLimits{records: 1, cost: maxHintCost, encoded: hintSnapshotBudget})
	first, second, third := testCacheKey(1), testCacheKey(2), testCacheKey(3)
	f.announce("A", first)
	f.announce("A", second) // FIFO forgets the first hint
	if f.hint("A", "A", first) != nil {
		t.Fatal("first hint survived FIFO eviction")
	}
	f.announce("A", third)
	if f.hint("A", "A", first) != nil {
		t.Fatal("an unrelated change reannounced a forgotten hint")
	}
	f.settle()
	var announced []string
	for _, r := range f.requests("A") {
		announced = append(announced, r.to)
	}
	if !slices.Equal(announced, []string{"B"}) {
		t.Fatalf("requests = %v", announced)
	}
	if f.hint("B", "A", third) == nil || f.hint("B", "A", first) != nil {
		t.Fatal("B did not receive exactly the retained hint")
	}
}
