package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
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
}

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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

	// Coalescing requires this caller's own accepted upstream GET. The first
	// attempt may then close its body and wait for another fill.
	resp, retry, err := t.roundTrip(req, true)
	if err != nil || !retry {
		return resp, err
	}

	// Timeout, fill failure, or eviction before reader acquisition left no cached
	// body. Repeat the full authorization/redirect path once, without waiting again.
	resp, _, err = t.roundTrip(req, false)
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

	// Seed variables for the following loop
	eligible := policy.eligible()
	permitted := policy.cacheable()
	current := req
	seen := map[string]bool{req.URL.String(): true}

	// Loop to follow redirect hops.
	for hops := 0; ; hops++ {

		// Single network round trip for this hop via the shared stdlib transport
		// (t.upstream); this registryTransport wraps it with retry, redirects, caching.
		resp, err := t.upstream.RoundTrip(current)
		if err != nil {
			return nil, false, err
		}

		// This hop is terminal: either not cache-eligible, or not a redirect to follow.
		// resolveResponse is the single exit — it meters the body and either bypasses
		// (ineligible) or caches (hit/fill) before returning the response to the client.
		if !eligible || !redirectStatus(resp.StatusCode) {
			return t.resolveResponse(req, resp, permitted, mayWait, stats)
		}

		// We're doing a redirect so drop the body.
		resp.Body.Close()

		// Erode permission: once any hop's headers forbid caching it stays forbidden for
		// the whole chain.
		permitted = permitted && cacheHeaders(resp.Header, object.digest)

		// Limit redirect depth to 5 hops
		if hops == 5 {
			return nil, false, errRedirect
		}

		// Resolve and validate Location. Refuse anything unsafe or already seen.
		next, err := redirectURL(current.URL, resp.Header.Get("Location"))
		if err != nil || seen[next.String()] {
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
