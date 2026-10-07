package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	announcePath         = "/v1/cache/announce"
	maxAnnouncementBytes = 512 << 10
	maxAnnouncementHints = 512
)

var errInvalidAnnouncement = errors.New("invalid announcement")

// announcement is one validated batch of hints from sender, addressed
// to recipient. Hints have distinct provider and key pairs.
type announcement struct {
	sender, recipient nodeID
	hints             []announcedHint
}

type announcementWire struct {
	SenderNodeID    string     `json:"senderNodeId"`
	RecipientNodeID string     `json:"recipientNodeId"`
	Hints           []hintWire `json:"hints"`
}

type hintWire struct {
	ProviderNodeID string   `json:"providerNodeId"`
	Key            string   `json:"key"`
	Seen           []string `json:"seen"`
}

// Exact field sets stop encoding/json's case-insensitive matching from
// accepting another spelling of a field.
var (
	announcementFields = map[string]bool{"senderNodeId": true, "recipientNodeId": true, "hints": true}
	hintFields         = map[string]bool{"providerNodeId": true, "key": true, "seen": true}
)

// encodeAnnouncement validates and bounds a message for the wire with the same
// rules decodeAnnouncement applies.
func encodeAnnouncement(message announcement) ([]byte, error) {
	if validateAnnouncement(message) != nil {
		return nil, errInvalidAnnouncement
	}
	wire := announcementWire{
		SenderNodeID:    message.sender.String(),
		RecipientNodeID: message.recipient.String(),
		Hints:           make([]hintWire, len(message.hints)),
	}
	for i, c := range message.hints {
		seen := make([]string, len(c.seen))
		for j, id := range c.seen {
			seen[j] = id.String()
		}
		wire.Hints[i] = hintWire{ProviderNodeID: c.provider.String(), Key: c.key.String(), Seen: seen}
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > maxAnnouncementBytes {
		return nil, errInvalidAnnouncement
	}
	return encoded, nil
}

// decodeAnnouncement strictly decodes and validates one message. It rejects
// unknown, duplicate, missing, or differently cased fields, wrong types,
// trailing data, and any invalid hint, so callers never act on part of a
// batch.
func decodeAnnouncement(encoded []byte) (announcement, error) {
	if len(encoded) > maxAnnouncementBytes || checkAnnouncementKeys(encoded) != nil {
		return announcement{}, errInvalidAnnouncement
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire announcementWire
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return announcement{}, errInvalidAnnouncement
	}
	sender, senderOK := parseNodeID(wire.SenderNodeID)
	recipient, recipientOK := parseNodeID(wire.RecipientNodeID)
	if !senderOK || !recipientOK || len(wire.Hints) > maxAnnouncementHints {
		return announcement{}, errInvalidAnnouncement
	}
	message := announcement{sender: sender, recipient: recipient, hints: make([]announcedHint, len(wire.Hints))}
	for i, c := range wire.Hints {
		parsed, ok := parseHint(c)
		if !ok {
			return announcement{}, errInvalidAnnouncement
		}
		message.hints[i] = parsed
	}
	if validateAnnouncement(message) != nil {
		return announcement{}, errInvalidAnnouncement
	}
	return message, nil
}

func parseHint(c hintWire) (announcedHint, bool) {
	provider, providerOK := parseNodeID(c.ProviderNodeID)
	key, keyOK := parseCacheKey(c.Key)
	if !providerOK || !keyOK || len(c.Seen) > maxHintSeen {
		return announcedHint{}, false
	}
	seen := make([]nodeID, len(c.Seen))
	for i, s := range c.Seen {
		id, ok := parseNodeID(s)
		if !ok {
			return announcedHint{}, false
		}
		seen[i] = id
	}
	return announcedHint{provider: provider, key: key, seen: seen}, true
}

// validateAnnouncement checks the semantic rules shared by both directions.
// IDs must be valid, and every seen set must be sorted, unique, bounded, and
// include both the provider and the immediate sender.
func validateAnnouncement(message announcement) error {
	if !validNodeID(message.sender.String()) || !validNodeID(message.recipient.String()) || message.sender == message.recipient || len(message.hints) == 0 || len(message.hints) > maxAnnouncementHints {
		return errInvalidAnnouncement
	}
	type pair struct {
		provider nodeID
		key      cacheKey
	}
	pairs := make(map[pair]struct{}, len(message.hints))
	for _, c := range message.hints {
		if !validNodeID(c.provider.String()) || len(c.seen) > maxHintSeen || !sortedUnique(c.seen) || !containsNodeID(c.seen, c.provider) || !containsNodeID(c.seen, message.sender) {
			return errInvalidAnnouncement
		}
		for _, id := range c.seen {
			if !validNodeID(id.String()) {
				return errInvalidAnnouncement
			}
		}
		if _, duplicate := pairs[pair{c.provider, c.key}]; duplicate {
			return errInvalidAnnouncement
		}
		pairs[pair{c.provider, c.key}] = struct{}{}
	}
	return nil
}

// checkAnnouncementKeys requires exactly one object with every field under its
// exact name, and hints as an array of exact hint objects whose seen value
// is an array of strings. It rejects duplicate keys without recursing into
// values that cannot fit the message shape.
func checkAnnouncementKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	fields := 0
	err := checkObjectKeys(decoder, announcementFields, func(key string) error {
		fields++
		if key != "hints" {
			return checkString(decoder)
		}
		return checkArray(decoder, func() error {
			hintFieldCount := 0
			err := checkObjectKeys(decoder, hintFields, func(key string) error {
				hintFieldCount++
				if key != "seen" {
					return checkString(decoder)
				}
				return checkArray(decoder, func() error { return checkString(decoder) })
			})
			if err != nil || hintFieldCount != len(hintFields) {
				return errInvalidAnnouncement
			}
			return nil
		})
	})
	if err != nil || fields != len(announcementFields) {
		return errInvalidAnnouncement
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidAnnouncement
	}
	return nil
}

// checkArray consumes one array, calling element for each member.
func checkArray(decoder *json.Decoder, element func() error) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errInvalidAnnouncement
	}
	for decoder.More() {
		if err := element(); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return errInvalidAnnouncement
	}
	return nil
}
