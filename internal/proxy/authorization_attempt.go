package proxy

import (
	"net/http"
	"time"
)

// grantAttempt tracks one in-flight authorization attempt across a request's
// redirect hops via narrow/settle/abandon. A nil *grantAttempt means grant
// caching does not apply, and every method is a safe no-op on a nil receiver.
type grantAttempt struct {
	cache           *authorizationCache
	entry           *authorizationEntry
	requestLifetime time.Duration
	digest          string
	deadline        time.Time
	repository      bool
}

// beginGrantAttempt registers this request's authorization attempt and, when a
// live grant permits it, returns a response served entirely from cache with no
// upstream I/O. A non-nil response is a cache hit the caller must return
// immediately, and the returned attempt is then nil.
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
	repositoryObject, _, repository := repositoryBearer(req)
	if repository {
		object = repositoryObject
	}

	// Only bearer repository grants cover ranges. Other credentials retain
	// their exact-request behavior.
	if ranged, _ := req.Context().Value(rangeRequestKey{}).(bool); ranged && !repository {
		return nil, nil
	}

	if t.authorization == nil || t.cache == nil || !cacheShaped && !repository {
		return nil, nil
	}

	// Derive how long this request permits a cached grant to be reused, never
	// exceeding a tighter lifetime inherited from an earlier hop.
	requestLifetime := authorizationLifetime(req.Header, t.authorization.lifetime(req.Header))
	if original, ok := req.Context().Value(authorizationLifetimeKey{}).(time.Duration); ok {
		requestLifetime = min(requestLifetime, original)
	}

	// Register this request's in-flight entry, obtaining any current grant.
	entry, grant := t.authorization.start(req)

	// Serve from a cached grant if reuse is allowed and we may wait.
	// Repository identity excludes request headers. Without tracking approval
	// age, a tighter request ceiling must get fresh upstream approval instead.
	if cacheShaped && permitted && requestLifetime > 0 && mayWait &&
		(!repository || requestLifetime >= t.authorization.lifetime(req.Header)) {
		if resp := t.authorizedResponse(req, entry, grant, stats); resp != nil {
			return resp, nil
		}
	}

	return nil, &grantAttempt{
		cache:           t.authorization,
		entry:           entry,
		requestLifetime: requestLifetime,
		digest:          object.digest,
		repository:      repository,
	}
}

// narrow folds this hop's freshness into the running deadline and abandons the
// grant outright if the hop forbids caching. It must run before settle so the
// deadline reflects the terminal hop. permitted is passed per hop because
// caching permission erodes across the redirect chain. Transient failures do
// not renew grants. Explicit repository reuse restrictions still invalidate.
func (a *grantAttempt) narrow(resp *http.Response, received time.Time, permitted bool) {
	if a == nil {
		return
	}

	if hasAuthenticationState(resp.Header) {
		a.cache.invalidate(a.entry)
		return
	}

	if a.repository && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		authorizationLifetime(resp.Header, a.cache.authorizationTTL) <= 0 || !cacheHeaders(resp.Header, a.digest)) {
		a.cache.invalidate(a.entry)
		return
	}

	// A missing object says nothing about repository pull permission, even
	// when this request forced revalidation or was conditional.
	if a.repository && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
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

	// Defer repository invalidation due to request restrictions until the
	// terminal outcome. A revalidation redirect can still end in a missing object.
	if !a.repository && (!permitted || lifetime <= 0 || !cacheHeaders(resp.Header, a.digest)) {
		a.cache.invalidate(a.entry)
	}
}

// settle commits a grant for an eligible terminal response, or invalidates the
// entry otherwise. It relies on narrow having run for the same hop, so the
// deadline and authentication-state restrictions have already been applied.
// Transient failures cannot establish a grant. This path establishes grants from
// immutable GETs. With shared mutable caching enabled, eligible mutable GETs and
// tag HEADs establish the same repository grant, also approving immutable hits.
func (a *grantAttempt) settle(req *http.Request, resp *http.Response, received time.Time, permitted bool) {
	if a == nil || isTransient(resp.StatusCode) {
		return
	}
	if a.repository && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		return
	}

	// Request-only restrictions require upstream approval for this delivery,
	// not revocation of an existing repository grant. Preserve its original
	// expiry after successful revalidation without creating or renewing one.
	// narrow already invalidated any response-side restrictions on every hop.
	if a.repository && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified) &&
		(!permitted || a.requestLifetime <= 0) {
		return
	}

	meta, ok := objectMetadata(req, resp)
	if ok && permitted && received.Before(a.deadline) {
		a.cache.commit(a.entry, &authorizationGrant{meta: meta, expires: a.deadline})
	} else if !a.repository || !permitted || a.requestLifetime <= 0 {
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
