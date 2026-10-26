package proxy

import (
	"net/http"
	"time"
)

// Shared data is independent of the producer's approval. Each reader must hold
// its own current repository grant, including after waiting for a capture.
func (t registryTransport) sharedMutableRoundTrip(req *http.Request, approvalKey [32]byte, lifetime time.Duration) (*http.Response, error) {
	key := t.authorization.responseFingerprint(req, true)
	stats, _ := req.Context().Value(statsKey{}).(*requestStats)
	if stats == nil {
		stats = &requestStats{cache: "bypass"}
	}

	requestLifetime := authorizationLifetime(req.Header, t.authorization.authorizationTTL)
	if original, ok := req.Context().Value(authorizationLifetimeKey{}).(time.Duration); ok {
		requestLifetime = min(requestLifetime, original)
	}

	approval, grant := t.authorization.lookup(approvalKey)
	var entry *mutableEntry
	var leader bool

	// Like immutable repository hits, a tighter request freshness ceiling
	// requires fresh approval rather than borrowing an older grant.
	if grant != nil && requestLifetime >= t.authorization.authorizationTTL {
		var resp *http.Response
		resp, entry, leader = t.mutable.acquire(req, key)
		if resp == nil && entry == nil {
			stats.admission = "mutable_capacity"
		}

		if resp != nil {
			if req.Context().Err() == nil && t.authorization.valid(approval, grant) {
				stats.cache = "hit"
				return resp, nil
			}
			resp.Body.Close()
		}

		if entry != nil && !leader && t.fillWaitTimeout > 0 {
			if resp, err := t.waitMutable(req, key, entry, stats, approval, grant); resp != nil || err != nil {
				return resp, err
			}
		}

		if !leader {
			entry = nil
		}
	} else {
		// Unapproved requests always authorize independently. Reserve an absent
		// response only, without reading or joining another client's capture.
		var refused bool
		entry, refused = t.mutable.reserve(key)
		if refused {
			stats.admission = "mutable_capacity"
		}
	}

	if err := req.Context().Err(); err != nil {
		if entry != nil {
			t.mutable.finish(entry, nil, nil, 0, time.Time{})
		}
		return nil, err
	}

	approval, _ = t.authorization.startKey(approvalKey)
	resp, _, err := t.roundTrip(req, false)
	received := time.Now()
	if err != nil {
		if entry != nil {
			t.mutable.finish(entry, nil, nil, 0, time.Time{})
		}
		return resp, err
	}

	t.invalidateMutableResponse(req, resp)

	// Approval is settled at upstream headers even when this request owns no
	// capture. Publication of response bytes still requires complete EOF.
	grantLifetime := authorizationLifetime(resp.Header, requestLifetime)
	if resp.StatusCode == http.StatusOK && mutableResponsePermitted(resp) && !headerHasToken(resp.Header, "Vary", "Authorization") && grantLifetime > 0 && req.Context().Err() == nil {
		t.authorization.commit(approval, &authorizationGrant{expires: received.Add(grantLifetime)})
	}

	if entry != nil && headerHasToken(resp.Header, "Vary", "Authorization") {
		t.mutable.finish(entry, nil, nil, 0, time.Time{})
		return resp, nil
	}
	return t.captureMutable(req, resp, entry, lifetime, received, stats), nil
}

// Denials and response reuse restrictions revoke this token's repository grant,
// blocking both mutable and immutable reuse. Shared bytes and other tokens'
// grants remain intact. Missing objects and transient failures alone do not revoke.
func mutableGrantRevoked(resp *http.Response, limit time.Duration) bool {
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		authorizationLifetime(resp.Header, limit) <= 0 || headerHasToken(resp.Header, "Vary", "*") || headerHasToken(resp.Header, "Vary", "Authorization")
}
