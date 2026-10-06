package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	availabilityPath     = "/v1/cache/availability"
	maxAvailabilityBytes = 512 << 10
	maxAvailabilityClaim = 512
)

var errInvalidAvailability = errors.New("invalid availability message")

// availabilityMessage is one validated batch of claims from sender, addressed
// to recipient. Claims have distinct owner and key pairs.
type availabilityMessage struct {
	sender, recipient nodeID
	claims            []hintClaim
}

type availabilityWire struct {
	SenderNodeID    string      `json:"senderNodeId"`
	RecipientNodeID string      `json:"recipientNodeId"`
	Claims          []claimWire `json:"claims"`
}

type claimWire struct {
	OwnerNodeID string   `json:"ownerNodeId"`
	Key         string   `json:"key"`
	Seen        []string `json:"seen"`
}

// Exact field sets stop encoding/json's case-insensitive matching from
// accepting another spelling of a field.
var (
	availabilityFields = map[string]bool{"senderNodeId": true, "recipientNodeId": true, "claims": true}
	claimFields        = map[string]bool{"ownerNodeId": true, "key": true, "seen": true}
)

// encodeAvailability validates and bounds a message for the wire with the same
// rules decodeAvailability applies.
func encodeAvailability(message availabilityMessage) ([]byte, error) {
	if validateAvailability(message) != nil {
		return nil, errInvalidAvailability
	}
	wire := availabilityWire{
		SenderNodeID:    message.sender.String(),
		RecipientNodeID: message.recipient.String(),
		Claims:          make([]claimWire, len(message.claims)),
	}
	for i, c := range message.claims {
		seen := make([]string, len(c.seen))
		for j, id := range c.seen {
			seen[j] = id.String()
		}
		wire.Claims[i] = claimWire{OwnerNodeID: c.owner.String(), Key: c.key.String(), Seen: seen}
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > maxAvailabilityBytes {
		return nil, errInvalidAvailability
	}
	return encoded, nil
}

// decodeAvailability strictly decodes and validates one message. It rejects
// unknown, duplicate, missing, or differently cased fields, wrong types,
// trailing data, and any invalid claim, so callers never act on part of a
// batch.
func decodeAvailability(encoded []byte) (availabilityMessage, error) {
	if len(encoded) > maxAvailabilityBytes || checkAvailabilityKeys(encoded) != nil {
		return availabilityMessage{}, errInvalidAvailability
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire availabilityWire
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return availabilityMessage{}, errInvalidAvailability
	}
	sender, senderOK := parseNodeID(wire.SenderNodeID)
	recipient, recipientOK := parseNodeID(wire.RecipientNodeID)
	if !senderOK || !recipientOK || len(wire.Claims) > maxAvailabilityClaim {
		return availabilityMessage{}, errInvalidAvailability
	}
	message := availabilityMessage{sender: sender, recipient: recipient, claims: make([]hintClaim, len(wire.Claims))}
	for i, c := range wire.Claims {
		claim, ok := parseClaim(c)
		if !ok {
			return availabilityMessage{}, errInvalidAvailability
		}
		message.claims[i] = claim
	}
	if validateAvailability(message) != nil {
		return availabilityMessage{}, errInvalidAvailability
	}
	return message, nil
}

func parseClaim(c claimWire) (hintClaim, bool) {
	owner, ownerOK := parseNodeID(c.OwnerNodeID)
	key, keyOK := parseCacheKey(c.Key)
	if !ownerOK || !keyOK || len(c.Seen) > maxHintSeen {
		return hintClaim{}, false
	}
	seen := make([]nodeID, len(c.Seen))
	for i, s := range c.Seen {
		id, ok := parseNodeID(s)
		if !ok {
			return hintClaim{}, false
		}
		seen[i] = id
	}
	return hintClaim{owner: owner, key: key, seen: seen}, true
}

// validateAvailability checks the semantic rules shared by both directions.
// IDs must be valid, and every seen set must be sorted, unique, bounded, and
// include both the owner and the immediate sender.
func validateAvailability(message availabilityMessage) error {
	if !validNodeID(message.sender.String()) || !validNodeID(message.recipient.String()) || message.sender == message.recipient || len(message.claims) == 0 || len(message.claims) > maxAvailabilityClaim {
		return errInvalidAvailability
	}
	type pair struct {
		owner nodeID
		key   cacheKey
	}
	pairs := make(map[pair]struct{}, len(message.claims))
	for _, c := range message.claims {
		if !validNodeID(c.owner.String()) || len(c.seen) > maxHintSeen || !sortedUnique(c.seen) || !containsNodeID(c.seen, c.owner) || !containsNodeID(c.seen, message.sender) {
			return errInvalidAvailability
		}
		for _, id := range c.seen {
			if !validNodeID(id.String()) {
				return errInvalidAvailability
			}
		}
		if _, duplicate := pairs[pair{c.owner, c.key}]; duplicate {
			return errInvalidAvailability
		}
		pairs[pair{c.owner, c.key}] = struct{}{}
	}
	return nil
}

// checkAvailabilityKeys requires exactly one object with every field under its
// exact name, and claims as an array of exact claim objects whose seen value
// is an array of strings. It rejects duplicate keys without recursing into
// values that cannot fit the message shape.
func checkAvailabilityKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	fields := 0
	err := checkObjectKeys(decoder, availabilityFields, func(key string) error {
		fields++
		if key != "claims" {
			return checkString(decoder)
		}
		return checkArray(decoder, func() error {
			claimFieldCount := 0
			err := checkObjectKeys(decoder, claimFields, func(key string) error {
				claimFieldCount++
				if key != "seen" {
					return checkString(decoder)
				}
				return checkArray(decoder, func() error { return checkString(decoder) })
			})
			if err != nil || claimFieldCount != len(claimFields) {
				return errInvalidAvailability
			}
			return nil
		})
	})
	if err != nil || fields != len(availabilityFields) {
		return errInvalidAvailability
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidAvailability
	}
	return nil
}

// checkArray consumes one array, calling element for each member.
func checkArray(decoder *json.Decoder, element func() error) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errInvalidAvailability
	}
	for decoder.More() {
		if err := element(); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return errInvalidAvailability
	}
	return nil
}
