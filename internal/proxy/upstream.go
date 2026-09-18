package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

type registryTransport struct{ p *Proxy }

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		// Count every connection-acquisition attempt (transport retries plus
		// DNS/dial/TLS failures before the GET); requests rejected earlier don't
		// count. Attach once so redirects and fill-wait retries inherit, not
		// stack, the hook.
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GetConn: func(string) {
				t.p.metrics.mu.Lock()
				t.p.metrics.upstream++
				t.p.metrics.mu.Unlock()
			},
		}))
	}

	resp, retry, err := t.roundTrip(req, true)
	if err != nil || !retry {
		return resp, err
	}

	resp, _, err = t.roundTrip(req, false)
	return resp, err
}

func (t registryTransport) roundTrip(req *http.Request, mayWait bool) (*http.Response, bool, error) {
	// Check if request is a cacheable digest-addressed blob/manifest
	object, eligible := immutableObjectRequest(req)

	// Recover the shared stats pointer stashed by the forwarding handler on the context so
	// our writes to it surface in that handler's metrics/log.
	stats, _ := req.Context().Value(statsKey{}).(*requestStats)
	// Default when absent from running without the handler (unit tests).
	if stats == nil {
		stats = &requestStats{cache: "bypass"}
	}

	// Recover eligibility/cacheability policy computed by the forwarding handler.
	policy, ok := req.Context().Value(policyKey{}).(requestPolicy)
	// Default when absent from running without the handler (unit tests).
	if !ok {
		policy = requestPolicy{eligible: eligible, cacheable: eligible}
	}

	// Preserve original bypass decisions even if Connection header stripping
	// removes a Range, conditional, or no-store header from the outgoing request.
	eligible = eligible && policy.eligible
	permitted := eligible && policy.cacheable

	// Follow redirects ourselves, tracking every URL seen so a redirect loop terminates.
	current := req
	seen := map[string]bool{req.URL.String(): true}

	for hops := 0; ; hops++ {
		resp, err := t.p.transport.RoundTrip(current)
		if err != nil {
			return nil, false, err
		}

		// Terminal response (non-redirect or ineligible): hand off to the caching layer.
		if !eligible || !redirectStatus(resp.StatusCode) {
			return t.p.cachedResponse(req, resp, permitted, mayWait, stats)
		}
		// A hop stays cacheable only if its own headers allow it; drop the body before the next hop.
		permitted = permitted && cacheHeaders(resp.Header, object.digest)
		resp.Body.Close()
		// Cap redirect depth to refuse pathological or malicious redirect chains.
		if hops == 5 {
			return nil, false, errRedirect
		}
		// Resolve and validate Location; refuse anything unsafe or already visited (loop).
		next, err := redirectURL(current.URL, resp.Header.Get("Location"))
		if err != nil || seen[next.String()] {
			return nil, false, errRedirect
		}
		seen[next.String()] = true
		current = redirectRequest(req.Context(), current, next)
	}
}

// redirectRequest builds the follow-up request for a redirect to next. A
// same-origin hop keeps the current headers; a cross-origin hop drops all
// headers except a safe allowlist, so custom registry authentication headers
// never reach a different origin. Referer is always removed.
func redirectRequest(ctx context.Context, current *http.Request, next *url.URL) *http.Request {
	out := current.Clone(ctx)
	out.URL, out.Host = next, next.Host
	if !sameOrigin(current.URL, next) {
		// An allowlist, not a list of known secret header names: custom
		// registry authentication headers must not reach storage either.
		out.Header = make(http.Header)
		for _, name := range []string{"Accept", "Accept-Encoding", "User-Agent"} {
			if values := current.Header.Values(name); len(values) > 0 {
				out.Header[name] = append([]string(nil), values...)
			}
		}
	}
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

// These non-public ranges include special-use space beyond RFC1918/ULA.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/3"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(a) {
			return false
		}
	}
	return true
}

func (p *Proxy) dialUpstream(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()
	a, err := hostport.Parse(address, "443")
	if err != nil {
		// The address is unparseable, so there is no safe host to name.
		return nil, p.refuseDial("", errDialAddress)
	}
	_, permitted := p.destinations[a.String()]
	ips, err := p.lookupIP(ctx, a.Host())
	if err != nil || len(ips) == 0 {
		return nil, p.refuseDial(a.String(), errDialResolve)
	}
	// Validate the entire answer before making any connection. Never resolve
	// the hostname again after validation (DNS rebinding).
	for _, ip := range ips {
		if ip.Zone != "" || (!permitted && !publicIP(ip.IP)) {
			return nil, p.refuseDial(a.String(), errDialForbidden)
		}
	}
	deadline, _ := ctx.Deadline() // WithTimeout above always supplies a deadline.
	for i, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		// Reserve a share for each remaining address. Recompute after every
		// failure so time unused by a fast refusal remains available to others.
		attempt, cancelAttempt := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(ips)-i))
		conn, err := p.dialIP(attempt, network, net.JoinHostPort(ip.IP.String(), a.Port()))
		cancelAttempt()
		if err == nil {
			return &idleReadWriteConn{Conn: conn, timeout: p.cfg.IdleTimeout}, nil
		}
	}
	return nil, p.refuseDial(a.String(), errDialConnect)
}

// refuseDial records the specific reason a dial was refused for operators and
// returns it. Every reason wraps errRedirect, so callers and the client-facing
// response are unaffected. Only the upstream authority (never resolved IPs,
// credentials, or client data) is logged, matching the existing request log.
func (p *Proxy) refuseDial(authority string, reason error) error {
	if p.logger != nil {
		p.logger.Debug("upstream dial refused", "authority", authority, "reason", reason)
	}
	return reason
}
