package peering

import (
	"slices"
	"testing"
)

func TestHintIndexMergesProvidersWithoutClearingClaims(t *testing.T) {
	x := newHintIndex(4)
	p1, p2 := testParsedNodeID(t, 1), testParsedNodeID(t, 2)
	x.merge(p2, []cacheKey{testCacheKey(1), testCacheKey(3)})
	x.merge(p1, []cacheKey{testCacheKey(1), testCacheKey(2), testCacheKey(1)})
	x.merge(p2, nil)
	x.merge(p1, []cacheKey{testCacheKey(4)})

	assertHintLen(t, x, 4)
	assertProviders(t, x, testCacheKey(1), testNodeID(1), testNodeID(2))
	assertProviders(t, x, testCacheKey(2), testNodeID(1))
	assertProviders(t, x, testCacheKey(3), testNodeID(2))
	assertProviders(t, x, testCacheKey(4), testNodeID(1))
}

func TestHintIndexEvictsWholeKeysWithoutReordering(t *testing.T) {
	x := newHintIndex(3)
	p1, p2 := testParsedNodeID(t, 1), testParsedNodeID(t, 2)
	x.merge(p1, []cacheKey{testCacheKey(1), testCacheKey(2), testCacheKey(3)})
	// Neither a repeated claim, another provider, nor a lookup saves key 1.
	x.merge(p1, []cacheKey{testCacheKey(1)})
	x.merge(p2, []cacheKey{testCacheKey(1)})
	assertProviders(t, x, testCacheKey(1), testNodeID(1), testNodeID(2))
	x.merge(p2, []cacheKey{testCacheKey(4)})

	assertHintLen(t, x, 3)
	assertProviders(t, x, testCacheKey(1))
	assertProviders(t, x, testCacheKey(2), testNodeID(1))
	assertProviders(t, x, testCacheKey(3), testNodeID(1))
	assertProviders(t, x, testCacheKey(4), testNodeID(2))
	// An evicted key can be admitted again and evicts the next oldest key.
	x.merge(p2, []cacheKey{testCacheKey(1)})
	assertProviders(t, x, testCacheKey(1), testNodeID(2))
	assertProviders(t, x, testCacheKey(2))
}

func TestHintIndexMergeOverLimitKeepsLastAdmissions(t *testing.T) {
	x := newHintIndex(3)
	p := testParsedNodeID(t, 1)
	x.merge(p, testCacheKeys(1, 5))

	assertHintLen(t, x, 3)
	for _, i := range []int{1, 2} {
		assertProviders(t, x, testCacheKey(i))
	}
	for _, i := range []int{3, 4, 5} {
		assertProviders(t, x, testCacheKey(i), testNodeID(1))
	}
}

func assertHintLen(t *testing.T, x *hintIndex, want int) {
	t.Helper()
	if got := len(x.byKey); got != want {
		t.Fatalf("len() = %d, want %d", got, want)
	}
}

func assertProviders(t *testing.T, x *hintIndex, key cacheKey, want ...string) {
	t.Helper()
	if got := x.providers(key); !slices.Equal(got, want) {
		t.Fatalf("providers(%s) = %q, want %q", key, got, want)
	}
}
