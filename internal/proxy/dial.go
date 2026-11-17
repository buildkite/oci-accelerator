package proxy

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/buildkite/go-crew-kit/outbound"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// dialUpstream is the upstream transport's dialer and its SSRF boundary.
// Destinations outside the configured registries and redirect allowlist may
// resolve only to public addresses, and zoned addresses are always refused. It
// dials the vetted IPs directly, leaving the hostname for TLS verification.
func (p *Proxy) dialUpstream(ctx context.Context, network, address string) (net.Conn, error) {
	a, err := hostport.Parse(address, "443")
	if err != nil {
		// The address is unparseable, so there is no safe host to name.
		return nil, p.refuseDial("", &outbound.Error{Kind: outbound.DestinationDenied, Err: err})
	}

	dialer := outbound.Dialer{
		Timeout:     p.cfg.DialTimeout,
		LookupNetIP: p.lookupNetIP,
		Dial:        p.dialIP,
		Authorize: func(_, _ string) (func(netip.Addr) bool, error) {
			if _, permitted := p.destinations[a.String()]; permitted {
				return func(netip.Addr) bool { return true }, nil
			}

			return nil, nil
		},
	}

	conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.Host(), a.Port()))
	if err != nil {
		return nil, p.refuseDial(a.String(), err)
	}

	return &idleReadWriteConn{Conn: conn, timeout: p.cfg.IdleTimeout}, nil
}

// refuseDial records the specific reason a dial was refused for users and
// returns it. Only the upstream authority (never resolved IPs,
// credentials, or client data) is logged, matching the existing request log.
func (p *Proxy) refuseDial(authority string, reason error) error {
	if p.logger != nil {
		p.logger.Debug("upstream dial refused", "authority", authority, "reason", upstreamFailure(reason))
	}

	return reason
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
