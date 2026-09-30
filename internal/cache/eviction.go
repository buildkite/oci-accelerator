package cache

import (
	"container/list"
	"sort"
)

// evictForAdmissionLocked reclaims space for an admission request. It removes at
// most maxAttempts victims per call and advances the store's shared resume cursor
// so later calls continue past this batch; maxAttempts must be positive. It reads
// the incrementally maintained eviction order. before prevents an older candidate
// from evicting newer entries. ErrBypass means insufficient eligible capacity in
// this batch. If removal failures prevent admission, return the first filesystem
// error. Only successful removals count as evictions. Repeated requests can
// reclaim more space, but competing traffic may consume it, so admission is not
// guaranteed; the shared cursor also makes eviction only approximately LRU.
func (s *Store) evictForAdmissionLocked(need int64, needSlots int, before int64, maxAttempts int) error {
	gather := func() []*entry { return s.evictionCandidatesLocked(before) }
	return s.evictLocked(need, needSlots, before, gather, maxAttempts)
}

// evictAtStartupLocked reclaims space during inventory reload with no per-call cap
// and no cursor interaction. It sorts the entry map directly because the ordered
// eviction list is not yet the source of truth at startup. before prevents an
// older reload candidate from evicting newer entries. Its ErrBypass and
// removal-error handling match evictForAdmissionLocked.
func (s *Store) evictAtStartupLocked(need int64, needSlots int, before int64) error {
	gather := func() []*entry { return s.sortedEntriesLocked(before, true) }
	return s.evictLocked(need, needSlots, before, gather, 0)
}

// evictLocked is the shared eviction core. gather supplies the ordered victim
// pool and is invoked only after the fast-path capacity check fails. maxAttempts
// > 0 caps victims per call and drives the resume cursor; maxAttempts == 0 leaves
// eviction uncapped and cursor-free.
func (s *Store) evictLocked(need int64, needSlots int, before int64, gather func() []*entry, maxAttempts int) error {
	fits := func() bool {
		free, freeSlots := s.freeCapacityLocked()
		return need <= free && needSlots <= freeSlots
	}
	if fits() {
		// A fitting admission must not reset another request's deferred batch.
		return nil
	}

	// Gather unpinned candidates no newer than before, and check that removing
	// all of them could even satisfy the request before disturbing anything.
	candidates := gather()
	if !s.batchSuffices(candidates, need, needSlots) {
		return ErrBypass
	}

	// Select the batch to remove. Admission eviction (maxAttempts > 0) removes at
	// most maxAttempts victims per call, resuming from the recorded cursor. The
	// sufficiency gate above guarantees at least one candidate here, so count >= 1
	// and the batch indexing below cannot go out of range or divide by zero.
	start, count := 0, len(candidates)
	if maxAttempts > 0 {
		count = min(count, maxAttempts)
		start = s.resumeStart(candidates)
		// Globally feasible requests may need several bounded batches. Resume
		// beyond this batch even when this response still bypasses storage.
		last := candidates[(start+count-1)%len(candidates)]
		s.evictionAfterLast, s.evictionAfter = last.last, last.meta.Key
	}

	// Hits can refresh later candidates while an earlier removal runs unlocked.
	// Each access replaces the list node, even if the clock repeats a timestamp.
	// Snapshot this batch's nodes so refreshed entries wait for a future batch.
	positions := make([]*list.Element, count)
	for i := range count {
		positions[i] = candidates[(start+i)%len(candidates)].eviction
	}

	// Remove victims until the request fits. A failed removal is retried later;
	// its charge stays booked, so keep the first error only if nothing frees up.
	var removeErr error
	for i := range count {
		if fits() {
			s.evictionAfter = ""
			return nil
		}
		e := candidates[(start+i)%len(candidates)]
		if s.entries[e.meta.Key] != e || e.pins != 0 || e.claimed || e.eviction != positions[i] || e.last > before {
			continue
		}
		e.claimed = true
		s.startOpLocked()
		s.mu.Unlock()
		err := s.removeAll(e.dir)
		s.mu.Lock()
		s.finishOpLocked()
		e.claimed = false
		if err == nil && s.entries[e.meta.Key] == e && e.pins == 0 {
			s.deleteEntryLocked(e)
			// Eviction only selects unpinned entries, so their charge is free.
			e.reclaimed = true
			s.bytes -= e.charge
			s.evictions++
		} else if err != nil && removeErr == nil {
			removeErr = err
		}
	}
	if fits() {
		s.evictionAfter = ""
		return nil
	}
	if removeErr != nil {
		return removeErr
	}
	return ErrBypass
}

func (s *Store) buildEvictionOrderLocked() {
	s.evictionOrder.Init()
	for _, e := range s.sortedEntriesLocked(int64(^uint64(0)>>1), false) {
		e.eviction = s.evictionOrder.PushBack(e)
	}
}

// addEntryLocked records e in both the key lookup and the incrementally
// maintained eviction order. Entries compare by (last, key).
func (s *Store) addEntryLocked(e *entry) {
	s.entries[e.meta.Key] = e
	s.insertEvictionLocked(e)
}

// Replace the list node even when last is unchanged: an in-flight eviction
// batch uses node identity to detect access while its mutex was released.
func (s *Store) setEntryLastLocked(e *entry, last int64) {
	if e.eviction != nil {
		s.evictionOrder.Remove(e.eviction)
	}
	e.last = last
	s.insertEvictionLocked(e)
}

func (s *Store) deleteEntryLocked(e *entry) {
	if e.eviction != nil {
		s.evictionOrder.Remove(e.eviction)
		e.eviction = nil
	}
	if s.entries[e.meta.Key] == e {
		delete(s.entries, e.meta.Key)
	}
}

// evictionCandidatesLocked returns the unpinned entries eligible for removal:
// those last accessed no later than before.
func (s *Store) evictionCandidatesLocked(before int64) []*entry {
	var candidates []*entry
	for item := s.evictionOrder.Front(); item != nil; item = item.Next() {
		e := item.Value.(*entry)
		if e.last > before {
			break
		}
		if e.pins == 0 && !e.claimed && e.last <= before {
			candidates = append(candidates, e)
		}
	}
	return candidates
}

// sortedEntriesLocked returns entries ordered by (last, key). Startup uses this
// both for uncapped pressure eviction and to bulk-build the live order after
// inventory, avoiding ordered insertion for every directory encountered.
func (s *Store) sortedEntriesLocked(before int64, unpinnedOnly bool) []*entry {
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.last <= before && (!unpinnedOnly || e.pins == 0 && !e.claimed) {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].last == entries[j].last {
			return entries[i].meta.Key < entries[j].meta.Key
		}
		return entries[i].last < entries[j].last
	})
	return entries
}

// batchSuffices reports whether removing every entry in batch would free enough
// capacity for the request, adding their reclaimable charge and slots to the
// current free capacity.
func (s *Store) batchSuffices(batch []*entry, need int64, needSlots int) bool {
	available, availableSlots := s.freeCapacityLocked()
	for _, e := range batch {
		available += e.charge
		availableSlots++
	}
	return available >= need && availableSlots >= needSlots
}

// resumeStart returns the index in candidates at which to resume, honoring the
// recorded (last, key) eviction cursor; zero when there is no cursor.
func (s *Store) resumeStart(candidates []*entry) int {
	if s.evictionAfter == "" {
		return 0
	}
	for i, e := range candidates {
		if e.last > s.evictionAfterLast || e.last == s.evictionAfterLast && e.meta.Key > s.evictionAfter {
			return i
		}
	}
	return 0
}

// freeCapacityLocked reports the currently unused bytes and entry slots.
func (s *Store) freeCapacityLocked() (bytes int64, slots int) {
	return s.max - s.bytes - s.reserved, s.maxEntries - len(s.entries) - len(s.fills) - len(s.claims) - len(s.failedCleanups)
}

func (s *Store) insertEvictionLocked(e *entry) {
	for item := s.evictionOrder.Back(); item != nil; item = item.Prev() {
		other := item.Value.(*entry)
		if other.last < e.last || other.last == e.last && other.meta.Key < e.meta.Key {
			e.eviction = s.evictionOrder.InsertAfter(e, item)
			return
		}
	}
	e.eviction = s.evictionOrder.PushFront(e)
}

func add(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > int64(^uint64(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}
