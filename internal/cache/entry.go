package cache

import "container/list"

// entry represents one committed generation of a cached object. Store.mu guards
// its mutable bookkeeping. A detached entry remains alive while readers pin it.
type entry struct {
	meta      Metadata      // identifies this generation by its key, size, and digest.
	dir       string        // filesystem path holding this generation's body and meta files.
	charge    int64         // byte count this entry contributes to the store's budget.
	last      int64         // logical access time in the live LRU order, seeded from commit-time rank at startup. In memory only.
	pins      int           // active readers holding the body open. A detached entry survives until this reaches zero.
	verified  bool          // set once the body's digest has been checked so readers can skip re-verification.
	verifying chan struct{} // non-nil while one Get verifies the body, and concurrent Gets wait on it. Guarded by Store.mu.
	invalid   bool          // retires this generation, excluding it from new lookups while existing readers finish.
	claimed   bool          // set while a synchronous removal currently owns this generation.
	reclaimed bool          // records that the charge was returned to the budget, done once after removal and the final unpin.
	eviction  *list.Element // this entry's node in the store's ordered LRU eviction list, nil once detached.
}
