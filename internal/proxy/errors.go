package proxy

import (
	"crypto/tls"
	"errors"

	"github.com/buildkite/go-crew-kit/outbound"
)

var errUpgrade = errors.New("upstream upgrade denied")

var errRedirect = errors.New("upstream redirect refused")

// upstreamFailure returns only fixed labels. Never log the original error: it
// can contain an upstream URL, signed query, certificate name or response data.
func upstreamFailure(err error) string {
	kind := outbound.Classify(err)
	switch kind {
	case outbound.Canceled:
		return "canceled"
	case outbound.Timeout:
		return "timeout"
	case outbound.DestinationDenied:
		return "dial_address"
	case outbound.DNS:
		return "dial_resolve"
	case outbound.AddressDenied:
		return "dial_forbidden"
	case outbound.Connect:
		return "dial_connect"
	}

	switch {
	case errors.Is(err, errRedirect):
		return "redirect"
	case errors.Is(err, errUpgrade):
		return "upgrade"
	}

	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_certificate"
	}

	if kind == outbound.TLS {
		return "tls"
	}

	return "transport"
}
