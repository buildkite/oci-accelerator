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

	if approvalKey, shared := t.mutableRepositoryKey(req); shared {
		return t.sharedMutableRoundTrip(req, approvalKey, lifetime)
	}

	key := t.authorization.exactFingerprint(req)
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
		if resp, err := t.waitMutable(req, key, entry, stats, nil, nil); resp != nil || err != nil {
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

	if err != nil {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp, err
	}
	return t.captureMutable(req, resp, entry, lifetime, received, stats), nil
}

func (t registryTransport) captureMutable(req *http.Request, resp *http.Response, entry *mutableEntry, lifetime time.Duration, received time.Time, stats *requestStats) *http.Response {
	if entry == nil {
		return resp
	}
	if req.Context().Err() != nil || !mutableResponsePermitted(resp) || (req.Method != http.MethodHead && resp.ContentLength > mutableMaxResponse) {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp
	}

	header := mutableHeaders(resp.Header)
	if mutableHeaderSize(header) > maxHeaderBytes {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp
	}

	expires := received.Add(lifetime)
	stats.cache = "miss"
	if req.Method == http.MethodHead {
		// HEAD describes a representation but carries no body to capture.
		t.mutable.finish(entry, header, nil, resp.ContentLength, expires)
		return resp
	}

	capacity := mutableMaxResponse
	if resp.ContentLength >= 0 {
		capacity = int(resp.ContentLength)
	}

	resp.Body = &mutableCapture{
		ReadCloser: resp.Body, cache: t.mutable, entry: entry, request: req,
		response: resp, header: header, data: make([]byte, 0, capacity), expires: expires,
	}
	return resp
}

func (t registryTransport) invalidateMutableResponse(req *http.Request, resp *http.Response) {
	if key, shared := t.mutableRepositoryKey(req); shared {
		if mutableGrantRevoked(resp, t.authorization.authorizationTTL) {
			t.authorization.invalidateKey(key)
		}
		return
	}

	if t.mutable == nil || t.authorization == nil || !mutableRequest(req) {
		return
	}

	// Match immutable authorization fencing: transport failures and transient
	// statuses do not revoke an approval, but authentication-state headers do.
	if hasAuthenticationState(resp.Header) || !isTransient(resp.StatusCode) && !mutableResponsePermitted(resp) {
		t.mutable.invalidate(t.authorization.exactFingerprint(req))
	}
}

func (t registryTransport) waitMutable(req *http.Request, key [32]byte, entry *mutableEntry, stats *requestStats, approval *authorizationEntry, grant *authorizationGrant) (*http.Response, error) {
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
			if approval == nil || t.authorization.valid(approval, grant) {
				stats.cache = "wait_hit"
				return resp, nil
			}
			resp.Body.Close()
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
