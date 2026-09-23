package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// registryTransport is the outer http.RoundTripper the reverse proxy calls. It
// adds the upstream-attempt metric, one retry, redirect following, and the
// caching hand-off, then delegates each individual network round trip to the
// shared upstream transport, which does the actual DNS/dial/TLS/HTTP.
type registryTransport struct {
	upstream        *http.Transport
	cache           *cache.Store
	metrics         *metrics
	fillWaitTimeout time.Duration
	authorization   *authorizationCache
	startRangeFetch func(context.Context) (*rangeFetch, error)
}

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clientRequest := req
	ranged, _ := req.Context().Value(rangeRequestKey{}).(bool)
	var fetch *rangeFetch
	if ranged {
		if full, ok := fullBlobRangeRequest(req); ok {
			req = full
		}
		var err error
		fetch, err = t.startRangeFetch(req.Context())
		if err != nil {
			return nil, err
		}
		req = req.WithContext(context.WithValue(fetch.ctx, rangeFetchKey{}, fetch))
	}
	if req.Method == http.MethodGet {
		// Count every connection-acquisition attempt (transport retries plus
		// DNS/dial/TLS failures before the GET); requests rejected earlier don't
		// count. Attach once so redirects and fill-wait retries inherit, not
		// stack, the hook.
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GetConn: func(string) {
				t.metrics.mu.Lock()
				t.metrics.upstream++
				t.metrics.mu.Unlock()
			},
		}))
	}

	// Make our own authorized GET first. This attempt may then close its body
	// and wait for another request's fill instead of downloading it again.
	resp, retry, err := t.roundTrip(req, true)
	if err == nil && retry {
		// Repeat the full authorization/redirect path once, without waiting again.
		resp, _, err = t.roundTrip(req, false)
	}
	if err != nil {
		if fetch != nil {
			fetch.finish()
		}
		return nil, err
	}
	if ranged {
		body, cached := resp.Body.(*cache.Reader)
		if !fetch.detached {
			resp.Body = &rangeFetchBody{ReadCloser: resp.Body, finish: fetch.finish}
		}
		if cached {
			resp = serveRangeResponse(clientRequest, resp, body)
		} else if _, eligible := objectMetadata(req, resp); eligible {
			// A streaming source cannot seek backwards for multipart delivery.
			// Like nginx, ignore multiple ranges on a cold response.
			selection := clientRequest
			if strings.Contains(selection.Header.Get("Range"), ",") {
				selection = selection.Clone(selection.Context())
				selection.Header.Del("Range")
			}
			resp = serveRangeResponse(selection, resp, &rangeStream{source: resp.Body, size: resp.ContentLength})
		}
	}
	return resp, err
}

func (t registryTransport) roundTrip(req *http.Request, mayWait bool) (*http.Response, bool, error) {
	// Parse the object and classify it from the request
	object, cacheShaped := immutableObjectRequest(req)

	// Recover the shared stats pointer stashed by the forwarding handler on the context so
	// our writes to it surface in that handler's metrics/log.
	stats, _ := req.Context().Value(statsKey{}).(*requestStats)
	// Absent only when invoked without that handler (unit tests): fall back to a bypass record.
	if stats == nil {
		stats = &requestStats{cache: "bypass"}
	}

	// Recover eligibility/cacheability policy computed by the forwarding handler.
	policy, ok := req.Context().Value(policyKey{}).(requestPolicy)

	// Absent only when invoked without that handler (unit tests): fall back to our own
	// shape check, treating any cache-shaped request as fully cacheable.
	if !ok {
		policy = policyBypass
		if cacheShaped {
			policy = policyCacheable
		}
	}

	// Preserve original bypass decisions even if Connection header stripping
	// removes a Range, conditional, or no-store header from the outgoing request.
	if !cacheShaped {
		policy = policyBypass
	}

	// Seed the eligibility, permission, and redirect-tracking state used below.
	eligible := policy.eligible()
	permitted := policy.cacheable()
	current := req
	seen := map[string]bool{req.URL.String(): true}

	// Fast path: serve from a cached authorization grant with no upstream I/O.
	// On a miss, attempt carries the in-flight entry for the upstream path below
	// to settle, or is nil when grant caching does not apply.
	resp, attempt := t.beginGrantAttempt(req, object, cacheShaped, permitted, mayWait, stats)
	if resp != nil {
		return resp, false, nil
	}

	// Upstream path: follow redirect hops, then hand off to resolveResponse.
	for hops := 0; ; hops++ {

		// Single network round trip for this hop via the shared stdlib transport
		// (t.upstream); this registryTransport wraps it with retry, redirects, caching.
		resp, err := t.upstream.RoundTrip(current)
		if err != nil {
			return nil, false, err
		}

		received := time.Now()

		// Narrow the grant deadline to this hop's freshness, abandoning it if the
		// hop forbids caching. Must precede settle so the deadline is set.
		attempt.narrow(resp, received, permitted)

		// This hop is terminal: either not cache-eligible, or not a redirect to follow.
		// resolveResponse is the single exit — it meters the body and either bypasses
		// (ineligible) or caches (hit/fill) before returning the response to the client.
		if !eligible || !redirectStatus(resp.StatusCode) {
			attempt.settle(req, resp, received, permitted)
			return t.resolveResponse(req, resp, permitted, mayWait, stats)
		}

		// We're doing a redirect so drop the body.
		resp.Body.Close()

		// Erode permission: once any hop's headers forbid caching it stays forbidden for
		// the whole chain.
		permitted = permitted && cacheHeaders(resp.Header, object.digest)

		// Limit redirect depth to 5 hops
		if hops == 5 {
			attempt.abandon()
			return nil, false, errRedirect
		}

		// Resolve and validate Location. Refuse anything unsafe or already seen.
		next, err := redirectURL(current.URL, resp.Header.Get("Location"))
		if err != nil || seen[next.String()] {
			attempt.abandon()
			return nil, false, errRedirect
		}

		seen[next.String()] = true
		current = buildRedirectRequest(req.Context(), current, next)
	}
}

// crossOriginSafeHeaders are the only request headers carried across a
// cross-origin redirect.
var crossOriginSafeHeaders = []string{"Accept", "Accept-Encoding", "User-Agent"}

// buildRedirectRequest builds the follow-up request for a redirect to next.
func buildRedirectRequest(ctx context.Context, current *http.Request, next *url.URL) *http.Request {
	out := current.Clone(ctx)
	out.URL, out.Host = next, next.Host

	// Only retain a safe allowlist of headers for cross-origin hops.
	if !sameOrigin(current.URL, next) {
		out.Header = make(http.Header)
		for _, name := range crossOriginSafeHeaders {
			if values := current.Header.Values(name); len(values) > 0 {
				out.Header[name] = append([]string(nil), values...)
			}
		}
	}

	// Always drop Referer header.
	out.Header.Del("Referer")
	return out
}

func redirectStatus(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// isTransient reports whether a status is a retryable upstream failure whose
// status alone must not disturb a cached authorization grant.
func isTransient(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func redirectURL(base *url.URL, location string) (*url.URL, error) {
	if location == "" {
		return nil, errRedirect
	}
	u, err := base.Parse(location)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return nil, errRedirect
	}
	a, err := hostport.Parse(u.Host, "443")
	if err != nil {
		return nil, errRedirect
	}
	u.Host = net.JoinHostPort(a.Host(), a.Port())
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	x, ex := hostport.Parse(a.Host, "443")
	y, ey := hostport.Parse(b.Host, "443")
	return ex == nil && ey == nil && a.Scheme == b.Scheme && x == y
}
