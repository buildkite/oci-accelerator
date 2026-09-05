package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/authority"
)

var errRedirect = errors.New("upstream redirect refused")

type registryTransport struct{ p *Proxy }

func (t registryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, retry, err := t.roundTrip(req, true)
	if err != nil || !retry {
		return resp, err
	}
	resp, _, err = t.roundTrip(req, false)
	return resp, err
}

func (t registryTransport) roundTrip(req *http.Request, mayWait bool) (*http.Response, bool, error) {
	object, eligible := immutableObjectRequest(req)
	stats, _ := req.Context().Value(statsKey{}).(*requestStats)
	if stats == nil {
		stats = &requestStats{cache: "bypass", eligible: eligible, cacheable: eligible}
	}
	// Preserve original bypass decisions even if Connection header stripping
	// removes a Range, conditional, or no-store header from the outgoing request.
	eligible = eligible && stats.eligible
	permitted := eligible && stats.cacheable
	current := req
	seen := map[string]bool{req.URL.String(): true}
	for hops := 0; ; hops++ {
		resp, err := t.p.transport.RoundTrip(current)
		if err != nil {
			return nil, false, err
		}
		if !eligible || !redirectStatus(resp.StatusCode) {
			cached, retry := t.p.cachedResponse(req, resp, permitted, mayWait, stats)
			if cached == nil && req.Context().Err() != nil {
				return nil, false, req.Context().Err()
			}
			return cached, retry, nil
		}
		permitted = permitted && cacheHeaders(resp.Header, object.digest)
		resp.Body.Close()
		if hops == 5 {
			return nil, false, errRedirect
		}
		next, err := redirectURL(current.URL, resp.Header.Get("Location"))
		if err != nil || seen[next.String()] {
			return nil, false, errRedirect
		}
		seen[next.String()] = true
		out := current.Clone(req.Context())
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
		current = out
	}
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
	a, err := authority.Parse(u.Host, "443")
	if err != nil {
		return nil, errRedirect
	}
	u.Host = net.JoinHostPort(a.Host(), a.Port())
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	x, ex := authority.Parse(a.Host, "443")
	y, ey := authority.Parse(b.Host, "443")
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
	a, err := authority.Parse(address, "443")
	if err != nil {
		return nil, errRedirect
	}
	_, permitted := p.destinations[a.String()]
	ips, err := p.lookupIP(ctx, a.Host())
	if err != nil || len(ips) == 0 {
		return nil, errRedirect
	}
	// Validate the entire answer before making any connection. Never resolve
	// the hostname again after validation (DNS rebinding).
	for _, ip := range ips {
		if ip.Zone != "" || (!permitted && !publicIP(ip.IP)) {
			return nil, errRedirect
		}
	}
	for _, ip := range ips {
		conn, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), a.Port()))
		if err == nil {
			return &idleReadWriteConn{Conn: conn, timeout: p.cfg.IdleTimeout}, nil
		}
	}
	return nil, errRedirect
}
