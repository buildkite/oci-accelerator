package proxy

import (
	"net/http"
	"time"
)

// Mutable requests retain their existing single-hop upstream behavior. Only a
// successful complete response may supply bytes and approval to another request.
func (t registryTransport) mutableRoundTrip(req *http.Request, lifetime time.Duration) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	key := t.authorization.fingerprint(req)
	stats, _ := req.Context().Value(statsKey{}).(*requestStats)
	if stats == nil {
		stats = &requestStats{cache: "bypass"}
	}

	resp, entry, leader := t.mutable.acquire(req, key)
	if resp != nil {
		stats.cache = "hit"
		return resp, nil
	}

	if entry != nil && !leader && t.fillWaitTimeout > 0 {
		if resp, err := t.waitMutable(req, key, entry, stats); resp != nil || err != nil {
			return resp, err
		}
	}

	// Fallbacks do not capture or replace the leading fetch. This keeps failures,
	// cancellations, and wait timeouts from producing competing cache publications.
	resp, _, err := t.roundTrip(req, false)
	received := time.Now()
	if err == nil {
		t.invalidateMutableResponse(req, resp)
	}
	if !leader {
		return resp, err
	}

	if err != nil || req.Context().Err() != nil || !mutableResponsePermitted(resp) || (req.Method != http.MethodHead && resp.ContentLength > mutableMaxResponse) {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp, err
	}

	header := mutableHeaders(resp.Header)
	if mutableHeaderSize(header) > maxHeaderBytes {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp, nil
	}

	expires := received.Add(lifetime)
	stats.cache = "miss"
	if req.Method == http.MethodHead {
		// HEAD describes a representation but carries no body to capture.
		t.mutable.finish(entry, header, nil, resp.ContentLength, expires)
		return resp, nil
	}

	capacity := mutableMaxResponse
	if resp.ContentLength >= 0 {
		capacity = int(resp.ContentLength)
	}
	resp.Body = &mutableCapture{
		ReadCloser: resp.Body, cache: t.mutable, entry: entry, request: req,
		response: resp, header: header, data: make([]byte, 0, capacity), expires: expires,
	}
	return resp, nil
}

func (t registryTransport) invalidateMutableResponse(req *http.Request, resp *http.Response) {
	if t.mutable == nil || t.authorization == nil || !mutableRequest(req) {
		return
	}

	// Match immutable authorization fencing: transport failures and transient
	// statuses do not revoke an approval, but authentication-state headers do.
	if hasAuthenticationState(resp.Header) || !isTransient(resp.StatusCode) && !mutableResponsePermitted(resp) {
		t.mutable.invalidate(t.authorization.fingerprint(req))
	}
}

func (t registryTransport) waitMutable(req *http.Request, key [32]byte, entry *mutableEntry, stats *requestStats) (*http.Response, error) {
	if t.metrics != nil {
		t.metrics.mu.Lock()
		t.metrics.activeWaits++
		t.metrics.mu.Unlock()
		defer func() { t.metrics.mu.Lock(); t.metrics.activeWaits--; t.metrics.mu.Unlock() }()
	}

	timer := time.NewTimer(t.fillWaitTimeout)
	defer timer.Stop()

	select {
	case <-req.Context().Done():
		stats.wait = "canceled"
		return nil, req.Context().Err()
	case <-entry.done:
		stats.wait = "complete"
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if resp := t.mutable.hit(req, key); resp != nil {
			stats.cache = "wait_hit"
			return resp, nil
		}
	case <-timer.C:
		stats.wait = "timeout"
	}

	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	stats.fallback = "retry"
	return nil, nil
}
