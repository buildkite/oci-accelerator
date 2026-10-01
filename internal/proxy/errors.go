package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
)

var errUpgrade = errors.New("upstream upgrade denied")

var errRedirect = errors.New("upstream redirect refused")

// Internal dial-refusal reasons. Each wraps errRedirect so existing
// errors.Is(err, errRedirect) checks and the client-facing 502 are unchanged.
// The distinct reason exists only to make operator logs actionable. None of
// these is ever surfaced to the client.
var (
	errDialAddress   = fmt.Errorf("%w: unparseable upstream address", errRedirect)
	errDialResolve   = fmt.Errorf("%w: upstream DNS resolution failed", errRedirect)
	errDialForbidden = fmt.Errorf("%w: upstream address not permitted", errRedirect)
	errDialConnect   = fmt.Errorf("%w: upstream connection failed", errRedirect)
)

// upstreamFailure returns only fixed labels. Never log the original error: it
// can contain an upstream URL, signed query, certificate name or response data.
func upstreamFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errDialAddress):
		return "dial_address"
	case errors.Is(err, errDialResolve):
		return "dial_resolve"
	case errors.Is(err, errDialForbidden):
		return "dial_forbidden"
	case errors.Is(err, errDialConnect):
		return "dial_connect"
	case errors.Is(err, errRedirect):
		return "redirect"
	case errors.Is(err, errUpgrade):
		return "upgrade"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_certificate"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		// Includes TLS handshake and response-header timeouts. Do not inspect
		// implementation-specific error text to distinguish these phases.
		return "timeout"
	}
	return "transport"
}
