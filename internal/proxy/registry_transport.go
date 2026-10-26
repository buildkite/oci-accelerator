package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// registryTransport is the reverse proxy's RoundTripper. It adds redirect
// following, grants, and caching to cache-shaped GETs. Writes and other
// bypassed requests make one upstream round trip returned to the client
// unchanged. Dependencies are explicit, never a Proxy reference.
type registryTransport struct {
	upstream        *http.Transport                            // shared DNS/dial/TLS/HTTP transport. Proxy owns its shutdown
	cache           *cache.Store                               // object store owned by New's caller (nil disables caching)
	metrics         *metrics                                   // request counters, including upstream connection attempts
	fillWaitTimeout time.Duration                              // wait for another request's fill before one retry. Zero disables
	authorization   *authorizationCache                        // local authorization grants (nil disables grant reuse)
	startRangeFetch func(context.Context) (*rangeFetch, error) // registers a range fetch bounded by fill timeout and shutdown
	fetchPeer       func(context.Context, cache.Metadata) (io.ReadCloser, error)
}

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clientRequest := req

	ranged, _ := req.Context().Value(rangeRequestKey{}).(bool)
	var fetch *rangeFetch

	// Fetch the full representation for this range request. A bearer repository
	// grant can approve it locally, otherwise it authorizes upstream.
	if ranged {
		if full, ok := fullBlobRangeRequest(req); ok {
			req = full
		}

		var err error
		fetch, err = t.startRangeFetch(req.Context())
		if err != nil {
			return nil, err
		}

		// Carry fetch on the context for resolveResponse (cache.go) to detach and pump the fill.
		req = req.WithContext(context.WithValue(fetch.ctx, rangeFetchKey{}, fetch))
	}

	// Make our own authorized GET first. This attempt may then close its body
	// and wait for another request's fill instead of downloading it again.
	resp, retry, err := t.roundTrip(req, true)
	if err == nil && retry {
		// Repeat the full authorization/redirect path once, without waiting again.
		resp, _, err = t.roundTrip(req, false)
	}

	// On failure, release this range fetch's in-flight lifecycle accounting.
	if err != nil {
		if fetch != nil {
			fetch.finish()
		}
		return nil, err
	}

	// Apply downstream range handling where the response supports it.
	if ranged {
		body, cached := resp.Body.(*cache.Reader)

		// Unless the fill owns the fetch, finish it when the response body closes.
		if !fetch.detached {
			resp.Body = &rangeFetchBody{ReadCloser: resp.Body, finish: fetch.finish}
		}

		if cached {
			// Cached body is seekable, so serve the exact requested range(s).
			resp = serveRangeResponse(clientRequest, resp, body)
		} else if _, eligible := objectMetadata(req, resp); eligible {
			// Select locally only from an admissible response. Anything else is
			// forwarded unchanged, usually as a full 200, without a second GET.
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
	seen := map[string]bool{redirectKey(req.URL): true}

	// Fast path: serve from a cached authorization grant with no upstream I/O.
	// On a miss, attempt carries the in-flight entry for the upstream path below
	// to settle, or is nil when grant caching does not apply.
	resp, attempt := t.beginGrantAttempt(req, object, cacheShaped, permitted, mayWait, stats)
	if resp != nil {
		return resp, false, nil
	}

	// Upstream path: follow redirect hops, then hand off to resolveResponse.
	for hops := 0; ; hops++ {

		// Scope origin instrumentation to this network request, so a peer
		// request made with req.Context cannot inherit the origin trace.
		networkRequest := current
		if current.Method == http.MethodGet {
			networkRequest = current.WithContext(httptrace.WithClientTrace(current.Context(), &httptrace.ClientTrace{
				GetConn: func(string) {
					t.metrics.mu.Lock()
					t.metrics.upstream++
					t.metrics.mu.Unlock()
				},
			}))
		}

		// Single network round trip for this hop via the shared stdlib transport
		// (t.upstream). This registryTransport wraps it with retry, redirects, caching.
		resp, err := t.upstream.RoundTrip(networkRequest)
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

		// Limit redirect depth to 5 hops. Eligible requests follow redirects even
		// when permission has eroded and the response cannot be stored.
		if hops == 5 {
			attempt.abandon()
			return nil, false, errRedirect
		}

		// Resolve and validate Location. Refuse anything unsafe or already seen.
		next, err := redirectURL(current.URL, resp.Header.Get("Location"))
		if err != nil || seen[redirectKey(next)] {
			attempt.abandon()
			return nil, false, errRedirect
		}

		seen[redirectKey(next)] = true
		current = buildRedirectRequest(req.Context(), current, next)
	}
}

// crossOriginSafeHeaders are the only request headers carried across a
// cross-origin redirect. An allowlist, not a denylist, keeps custom credential
// headers from leaking along with the standard Authorization and Cookie.
var crossOriginSafeHeaders = []string{"Accept", "Accept-Encoding", "User-Agent"}

// buildRedirectRequest builds the follow-up request for a redirect to next.
// Stripping applies to current, so credentials are not restored if a later
// hop returns to the original origin.
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

	// Validate the authority but keep it as written. Presigned storage URLs can
	// sign the Host header, so adding a default port would invalidate them.
	if _, err := urlEndpoint(u); err != nil {
		return nil, errRedirect
	}
	return u, nil
}

// redirectKey identifies a redirect target for loop detection, treating an
// explicit default port and host case differences as the same target. Other
// spellings of the same resource, such as escaping variants, get distinct keys
// and are bounded by the redirect hop limit instead.
func redirectKey(u *url.URL) string {
	a, err := urlEndpoint(u)
	if err != nil {
		// Defensive: redirectURL and the pinned registry authority are already valid.
		return u.String()
	}
	return u.Scheme + "://" + a.String() + u.RequestURI()
}

// sameOrigin reports whether a and b share a scheme and dialed endpoint. It
// reports false for an ambiguous authority, so credentials are stripped.
func sameOrigin(a, b *url.URL) bool {
	x, ex := urlEndpoint(a)
	y, ey := urlEndpoint(b)
	return ex == nil && ey == nil && a.Scheme == b.Scheme && x == y
}

// urlEndpoint returns the canonical authority of a URL the caller has already
// required to be https, refusing one that hostport reads differently from the
// endpoint net/http dials. Comparing such an authority could keep credentials
// for another host. For example, the unbracketed "2001:db8::1:443" parses as
// one IPv6 address but dials [2001:db8::1]:443. A non-ASCII host such as
// "ghcr.İo" lowercases to "ghcr.io" but net/http dials its punycode form.
func urlEndpoint(u *url.URL) (hostport.HostPort, error) {
	// Refusing non-ASCII stands in for net/http's IDNA conversion.
	for i := 0; i < len(u.Host); i++ {
		if u.Host[i] >= utf8.RuneSelf {
			return hostport.HostPort{}, errRedirect
		}
	}

	authority, err := hostport.Parse(u.Host, "443")
	if err != nil {
		return hostport.HostPort{}, errRedirect
	}

	// Mirror the transport's dial address: Hostname and Port, defaulting to 443.
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialed, err := hostport.Parse(net.JoinHostPort(u.Hostname(), port), "443")
	if err != nil || dialed != authority {
		return hostport.HostPort{}, errRedirect
	}
	return authority, nil
}
