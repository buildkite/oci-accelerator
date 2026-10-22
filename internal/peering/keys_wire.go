package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const (
	keysPath = "/v1/cache/keys"
	// maxKeysPerPage keeps a full page well within maxKeysPageBytes. It is
	// part of the wire contract, because clients require every page but the
	// last to hold exactly this many keys.
	maxKeysPerPage   = 4096
	maxKeysPageBytes = 512 << 10
)

var errInvalidKeysPage = errors.New("invalid cache keys page")

// keysPageFields lists the exact top-level keys of a page. Exact matching
// stops encoding/json's case-insensitive field matching from accepting
// another spelling of a field.
var keysPageFields = map[string]bool{"nodeId": true, "keys": true, "next": true}

// keysPage is a page of unique keys in provider-owned recency order. A nonzero
// next identifies the last key's position when more keys follow. Keys can
// repeat across pages when live accesses move entries later.
type keysPage struct {
	node nodeID
	keys []cacheKey
	next cache.KeyPosition
}

type keysPageWire struct {
	NodeID string   `json:"nodeId"`
	Keys   []string `json:"keys"`
	Next   string   `json:"next"`
}

// encodeKeysPage validates and bounds a page for the wire with the same rules
// decodeKeysPage applies.
func encodeKeysPage(page keysPage) ([]byte, error) {
	if validateKeysPage(page) != nil {
		return nil, errInvalidKeysPage
	}
	wire := keysPageWire{NodeID: page.node.String(), Keys: make([]string, len(page.keys))}
	for i, key := range page.keys {
		wire.Keys[i] = key.String()
	}
	if page.next != (cache.KeyPosition{}) {
		wire.Next, _ = encodeKeysCursor(page.next) // validated above
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > maxKeysPageBytes {
		return nil, errInvalidKeysPage
	}
	return encoded, nil
}

// decodeKeysPage strictly decodes and validates one page. It rejects unknown,
// duplicate, missing, or differently cased fields, wrong types, trailing data,
// invalid keys or node IDs, duplicate keys, and invalid continuation positions,
// so callers never act on part of a page. Only the provider knows key recency.
func decodeKeysPage(encoded []byte) (keysPage, error) {
	if len(encoded) > maxKeysPageBytes || checkKeysPageKeys(encoded) != nil {
		return keysPage{}, errInvalidKeysPage
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire keysPageWire
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return keysPage{}, errInvalidKeysPage
	}
	node, ok := parseNodeID(wire.NodeID)
	if !ok || len(wire.Keys) > maxKeysPerPage {
		return keysPage{}, errInvalidKeysPage
	}
	page := keysPage{node: node, keys: make([]cacheKey, len(wire.Keys))}
	for i, text := range wire.Keys {
		if page.keys[i], ok = parseCacheKey(text); !ok {
			return keysPage{}, errInvalidKeysPage
		}
	}
	if wire.Next != "" {
		next, err := decodeKeysCursor(wire.Next)
		if err != nil {
			return keysPage{}, errInvalidKeysPage
		}
		page.next = next
	}
	if validateKeysPage(page) != nil {
		return keysPage{}, errInvalidKeysPage
	}
	return page, nil
}

// validateKeysPage checks the rules shared by both directions. A page names a
// valid node and holds at most maxKeysPerPage unique keys. Every nonterminal
// page is full and its cursor identifies its final key.
func validateKeysPage(page keysPage) error {
	if !validNodeID(page.node.String()) || len(page.keys) > maxKeysPerPage {
		return errInvalidKeysPage
	}
	if page.next != (cache.KeyPosition{}) {
		if len(page.keys) != maxKeysPerPage || page.next.Key != page.keys[len(page.keys)-1].String() {
			return errInvalidKeysPage
		}
		if _, err := encodeKeysCursor(page.next); err != nil {
			return errInvalidKeysPage
		}
	}
	seen := make(map[cacheKey]bool, len(page.keys))
	for _, key := range page.keys {
		if seen[key] {
			return errInvalidKeysPage
		}
		seen[key] = true
	}
	return nil
}

// checkKeysPageKeys requires exactly one top-level JSON object with every
// field present under its exact name, no duplicate keys, string scalars, and a
// flat array of strings for keys.
func checkKeysPageKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	fields := 0
	err := checkObjectKeys(decoder, keysPageFields, func(key string) error {
		fields++
		if key != "keys" {
			return checkString(decoder)
		}
		return checkStringArray(decoder)
	})
	if err != nil || fields != len(keysPageFields) {
		return errInvalidKeysPage
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidKeysPage
	}
	return nil
}

// checkStringArray consumes an array of strings, rejecting null and nested
// values.
func checkStringArray(decoder *json.Decoder) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errInvalidKeysPage
	}
	for decoder.More() {
		if err := checkString(decoder); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return errInvalidKeysPage
	}
	return nil
}
