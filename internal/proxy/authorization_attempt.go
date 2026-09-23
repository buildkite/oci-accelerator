package proxy

import (
	"net/http"
	"time"
)

// grantAttempt tracks one in-flight authorization attempt across a request's
// redirect hops via narrow/settle/abandon. A nil *grantAttempt means grant
// caching does not apply; every method is a safe no-op on a nil receiver.
type grantAttempt struct {
	cache           *authorizationCache
	entry           *authorizationEntry
	requestLifetime time.Duration
	digest          string
	deadline        time.Time
}

// beginGrantAttempt registers this request's authorization attempt and, when a
// live grant permits it, returns a response served entirely from cache with no
// upstream I/O. A non-nil response is a cache hit the caller must return
// immediately; the returned attempt is then nil.
//
// A nil response means the caller must proceed to the upstream path. The
// returned attempt carries the registered in-flight entry that the caller must
// later settle, or is nil when grant caching does not apply. A cache miss still
// returns a non-nil attempt: the entry must remain registered so a terminal
// success can commit a grant.
//
// Use the forwarded request's shape to register even an original bypass whose
// Range or conditional headers were stripped. Its response can invalidate a
// matching grant, but permitted remains false so it cannot serve or commit one.
func (t registryTransport) beginGrantAttempt(req *http.Request, object immutableObject, cacheShaped, permitted, mayWait bool, stats *requestStats) (*http.Response, *grantAttempt) {
	if ranged, _ := req.Context().Value(rangeRequestKey{}).(bool); ranged {
		return nil, nil
	}
	if t.authorization == nil || t.cache == nil || !cacheShaped {
		return nil, nil
	}

	// Derive how long this request permits a cached grant to be reused, never
	// exceeding a tighter lifetime inherited from an earlier hop.
	requestLifetime := authorizationLifetime(req.Header, t.authorization.ttl)
	if original, ok := req.Context().Value(authorizationLifetimeKey{}).(time.Duration); ok {
		requestLifetime = min(requestLifetime, original)
	}

	// Register this request's in-flight entry, obtaining any current grant.
	entry, grant := t.authorization.start(req)

	// Serve from a cached grant if reuse is allowed and we may wait.
	if permitted && requestLifetime > 0 && mayWait {
		if resp := t.authorizedResponse(req, entry, grant, stats); resp != nil {
			return resp, nil
		}
	}

	return nil, &grantAttempt{
		cache:           t.authorization,
		entry:           entry,
		requestLifetime: requestLifetime,
		digest:          object.digest,
	}
}

// narrow folds this hop's freshness into the running deadline and abandons the
// grant outright if the hop forbids caching. It must run before settle so the
// deadline reflects the terminal hop. permitted is passed per hop because
// caching permission erodes across the redirect chain. Transient failures are
// ignored unless they carry authentication-state headers.
func (a *grantAttempt) narrow(resp *http.Response, received time.Time, permitted bool) {
	if a == nil {
		return
	}
	if hasAuthenticationState(resp.Header) {
		a.cache.invalidate(a.entry)
		return
	}
	if isTransient(resp.StatusCode) {
		return
	}

	lifetime := authorizationLifetime(resp.Header, a.requestLifetime)
	hopDeadline := received.Add(lifetime)

	if a.deadline.IsZero() || hopDeadline.Before(a.deadline) {
		a.deadline = hopDeadline
	}

	if !permitted || lifetime <= 0 || !cacheHeaders(resp.Header, a.digest) {
		a.cache.invalidate(a.entry)
	}
}

// settle commits a grant for an eligible terminal response, or invalidates the
// entry otherwise. It relies on narrow having run for the same hop, so the
// deadline and authentication-state restrictions have already been applied.
// Transient failures cannot establish a grant.
func (a *grantAttempt) settle(req *http.Request, resp *http.Response, received time.Time, permitted bool) {
	if a == nil || isTransient(resp.StatusCode) {
		return
	}
	meta, ok := objectMetadata(req, resp)
	if ok && permitted && received.Before(a.deadline) {
		a.cache.commit(a.entry, &authorizationGrant{meta: meta, expires: a.deadline})
	} else {
		a.cache.invalidate(a.entry)
	}
}

// abandon invalidates any cached authorization for this attempt's identity,
// used when the redirect chain cannot reach an eligible terminal response.
func (a *grantAttempt) abandon() {
	if a == nil {
		return
	}
	a.cache.invalidate(a.entry)
}
