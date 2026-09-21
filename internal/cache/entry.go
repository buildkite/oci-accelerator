package cache

import "container/list"

// entry represents one committed generation of a cached object. Store.mu guards
// its mutable bookkeeping. A detached entry remains alive while readers pin it.
type entry struct {
	meta      Metadata
	dir       string
	charge    int64
	last      int64
	pins      int
	verified  bool
	verifying chan struct{} // Non-nil while one Get verifies; guarded by Store.mu.
	invalid   bool
	claimed   bool // A synchronous removal owns this generation.
	// Charges are reclaimed once, after directory removal and the final unpin.
	reclaimed bool
	eviction  *list.Element
}
