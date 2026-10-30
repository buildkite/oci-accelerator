package peering

import (
	"errors"
	"net"

	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// MaxPeers bounds confirmed members excluding self, configured seeds, pending
// verifications, and peer records in exchange messages and persisted state.
const MaxPeers = 32

var errInvalidMessage = errors.New("invalid peer message")

// Message is the complete peer list advertised by one peer. Peers maps each
// canonical peer address to the node ID the sender last associated with it.
type Message struct {
	Address string            `json:"address"`
	NodeID  string            `json:"nodeId"`
	Peers   map[string]string `json:"peers"`
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

// canonicalMessage validates every address and node ID and canonicalizes the
// addresses. Node IDs are case-sensitive and kept as is. Aliases of the same
// canonical address collapse when they agree and invalidate the message when
// they disagree. Records about the sender or the receiver are not checked
// against the sender's own mapping because the sender's mapping takes
// precedence when the message is merged.
func canonicalMessage(message Message) (Message, error) {
	if len(message.Peers) > MaxPeers || !validNodeID(message.NodeID) {
		return Message{}, errInvalidMessage
	}

	address, err := CanonicalAddress(message.Address)
	if err != nil {
		return Message{}, errInvalidMessage
	}

	peers := make(map[string]string, len(message.Peers))
	for peer, id := range message.Peers {
		canonical, err := CanonicalAddress(peer)
		if err != nil || !validNodeID(id) {
			return Message{}, errInvalidMessage
		}
		if existing, ok := peers[canonical]; ok && existing != id {
			return Message{}, errInvalidMessage
		}
		peers[canonical] = id
	}
	return Message{Address: address, NodeID: message.NodeID, Peers: peers}, nil
}
