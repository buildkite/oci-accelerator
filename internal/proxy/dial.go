package proxy

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// dialUpstream is the upstream transport's dialer and its SSRF boundary.
// Destinations outside the configured registries and redirect allowlist may
// resolve only to public addresses, and zoned addresses are always refused. It
// dials the vetted IPs directly, leaving the hostname for TLS verification.
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

	// Try each address in turn, dividing the remaining dial budget evenly among the addresses not yet attempted.
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

type idleReadWriteConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleReadWriteConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
}
func (c *idleReadWriteConn) Write(b []byte) (int, error) {
	if c.timeout > 0 {
		// The transport may already be reading on a pooled connection. Give
		// that pending read a fresh budget when sending the next request.
		_ = c.SetDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(b)
}
