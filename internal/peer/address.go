package peer

import (
	"errors"
	"net"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// MaxPeers is the maximum number of peer addresses in an exchange message.
const MaxPeers = 1024

var errInvalidMessage = errors.New("invalid peer message")

// Message is the complete peer list advertised by one peer.
type Message struct {
	Address string   `json:"address"`
	Peers   []string `json:"peers"`
}

// CanonicalAddress validates and canonicalizes an explicitly ported address.
func CanonicalAddress(address string) (string, error) {
	parsed, err := hostport.Parse(address, "")
	if err != nil {
		return "", err
	}
	host := parsed.Host()
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return "", errInvalidMessage
	}
	return net.JoinHostPort(host, parsed.Port()), nil
}

func canonicalMessage(message Message) (Message, error) {
	if len(message.Peers) > MaxPeers {
		return Message{}, errInvalidMessage
	}
	address, err := CanonicalAddress(message.Address)
	if err != nil {
		return Message{}, errInvalidMessage
	}
	peers := make([]string, len(message.Peers))
	for i, peer := range message.Peers {
		peers[i], err = CanonicalAddress(peer)
		if err != nil {
			return Message{}, errInvalidMessage
		}
	}
	return Message{Address: address, Peers: peers}, nil
}
