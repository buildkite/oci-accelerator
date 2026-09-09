package cache

import "sort"

// addEntryLocked records e in both the key lookup and the incrementally
// maintained eviction order. Entries compare by (last, key).
func (s *Store) addEntryLocked(e *entry) {
	s.entries[e.meta.Key] = e
	s.insertEvictionLocked(e)
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

// sortedEntriesLocked returns entries ordered by (last, key). Startup uses this
// both for uncapped pressure eviction and to bulk-build the live order after
// inventory, avoiding ordered insertion for every directory encountered.
func (s *Store) sortedEntriesLocked(before int64, unpinnedOnly bool) []*entry {
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.last <= before && (!unpinnedOnly || e.pins == 0) {
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

func (s *Store) buildEvictionOrderLocked() {
	s.evictionOrder.Init()
	for _, e := range s.sortedEntriesLocked(int64(^uint64(0)>>1), false) {
		e.eviction = s.evictionOrder.PushBack(e)
	}
}
