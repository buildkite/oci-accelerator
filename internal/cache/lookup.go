package cache

// Lookup returns metadata for the sole live committed representation associated
// with requestKey. Missing, ambiguous, malformed, and closed lookups return
// ErrMiss. It does not pin or verify the body, so a subsequent Get can still miss.
func (s *Store) Lookup(requestKey string) (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(requestKey) != keyHexLen || !isLowerHex(requestKey) {
		return Metadata{}, ErrMiss
	}
	candidates := s.requests[requestKey]
	if len(candidates) != 1 {
		return Metadata{}, ErrMiss
	}
	for _, e := range candidates {
		if !e.invalid && !e.claimed && s.entries[e.meta.Key] == e {
			return e.meta, nil
		}
	}
	return Metadata{}, ErrMiss
}

// Request associations contain only eligible entries. Maintaining eligibility
// when invalidating or claiming entries keeps Lookup constant-time even if one
// request has many representations or tombstones. All helpers require Store.mu.
func (s *Store) addRequestLocked(e *entry) {
	if e.meta.RequestKey == "" || e.invalid || e.claimed {
		return
	}
	candidates := s.requests[e.meta.RequestKey]
	if candidates == nil {
		candidates = make(map[string]*entry)
		s.requests[e.meta.RequestKey] = candidates
	}
	candidates[e.meta.Key] = e
}

func (s *Store) deleteRequestLocked(e *entry) {
	candidates := s.requests[e.meta.RequestKey]
	if candidates[e.meta.Key] == e {
		delete(candidates, e.meta.Key)
		if len(candidates) == 0 {
			delete(s.requests, e.meta.RequestKey)
		}
	}
}

func (s *Store) invalidateEntryLocked(e *entry) {
	e.invalid = true
	s.deleteRequestLocked(e)
}
