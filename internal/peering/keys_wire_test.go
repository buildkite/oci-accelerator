package peering

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestKeysCursorWireFormat(t *testing.T) {
	position := cache.KeyPosition{
		Incarnation: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Last:        0x0102030405060708,
		Key:         "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	}
	// Independently encoded version byte, incarnation, big-endian position, key.
	const token = "AQECAwQFBgcICQoLDA0ODxABAgMEBQYHCAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
	if got, err := encodeKeysCursor(position); err != nil || got != token {
		t.Fatalf("encoded cursor = %q, %v, want %q", got, err, token)
	}
	if got, err := decodeKeysCursor(token); err != nil || got != position {
		t.Fatalf("decoded cursor = %+v, %v, want %+v", got, err, position)
	}
}

func TestKeysPageRoundTrip(t *testing.T) {
	tests := map[string]keysPage{
		"empty last page":      {node: testParsedNodeID(t, 1), keys: []cacheKey{}},
		"nonlexical last page": {node: testParsedNodeID(t, 1), keys: []cacheKey{testCacheKey(2), testCacheKey(1)}},
		"full page":            {node: testParsedNodeID(t, 1), keys: testCacheKeys(0, maxKeysPerPage), next: testKeyPosition(maxKeysPerPage, testCacheKey(maxKeysPerPage-1))},
	}
	for name, page := range tests {
		encoded, err := encodeKeysPage(page)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		decoded, err := decodeKeysPage(encoded)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !reflect.DeepEqual(decoded, page) {
			t.Fatalf("%s: round trip = %+v, want %+v", name, decoded, page)
		}
	}
}

func TestKeysPageEncodesEmptyKeys(t *testing.T) {
	encoded, err := encodeKeysPage(keysPage{node: testParsedNodeID(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":""}`, testNodeID(1)); string(encoded) != want {
		t.Fatalf("empty page = %s, want %s", encoded, want)
	}
}

func TestEncodeKeysPageRejectsInvalidPages(t *testing.T) {
	node := testParsedNodeID(t, 1)
	tests := map[string]keysPage{
		"nonadjacent duplicate":  {node: node, keys: []cacheKey{testCacheKey(1), testCacheKey(2), testCacheKey(1)}},
		"more without keys":      {node: node, next: testKeyPosition(1, testCacheKey(1))},
		"short nonterminal page": {node: node, keys: []cacheKey{testCacheKey(1)}, next: testKeyPosition(1, testCacheKey(1))},
		"cursor not final key":   {node: node, keys: testCacheKeys(0, maxKeysPerPage), next: testKeyPosition(maxKeysPerPage, testCacheKey(1))},
		"zero cursor position":   {node: node, keys: testCacheKeys(0, maxKeysPerPage), next: testKeyPosition(0, testCacheKey(maxKeysPerPage-1))},
		"too many keys":          {node: node, keys: testCacheKeys(0, maxKeysPerPage+1)},
		"invalid node bytes":     {keys: []cacheKey{testCacheKey(1)}},
	}
	for name, page := range tests {
		if _, err := encodeKeysPage(page); err == nil {
			t.Errorf("%s: encode succeeded", name)
		}
	}
}

func TestDecodeKeysPageRejectsMalformedPages(t *testing.T) {
	node, k1, k2 := testNodeID(1), testCacheKey(1).String(), testCacheKey(2).String()
	valid := fmt.Sprintf(`{"nodeId":%q,"keys":[%q,%q],"next":""}`, node, k1, k2)
	if _, err := decodeKeysPage([]byte(valid)); err != nil {
		t.Fatalf("valid page rejected: %v", err)
	}
	tooMany := make([]string, maxKeysPerPage+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%q", testCacheKey(i).String())
	}
	tests := map[string]string{
		"empty":             ``,
		"not an object":     `[]`,
		"trailing data":     valid + ` {}`,
		"unknown field":     strings.TrimSuffix(valid, "}") + `,"extra":1}`,
		"missing next":      fmt.Sprintf(`{"nodeId":%q,"keys":[]}`, node),
		"missing keys":      fmt.Sprintf(`{"nodeId":%q,"next":""}`, node),
		"duplicate field":   fmt.Sprintf(`{"nodeId":%q,"nodeId":%q,"keys":[],"next":""}`, node, node),
		"wrong case":        fmt.Sprintf(`{"nodeID":%q,"keys":[],"next":""}`, node),
		"null keys":         fmt.Sprintf(`{"nodeId":%q,"keys":null,"next":""}`, node),
		"null next":         fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":null}`, node),
		"nested key":        fmt.Sprintf(`{"nodeId":%q,"keys":[[%q]],"next":""}`, node, k1),
		"numeric key":       fmt.Sprintf(`{"nodeId":%q,"keys":[1],"next":""}`, node),
		"invalid node":      `{"nodeId":"short","keys":[],"next":""}`,
		"uppercase key":     fmt.Sprintf(`{"nodeId":%q,"keys":[%q],"next":""}`, node, strings.Repeat("AB", 32)),
		"short key":         fmt.Sprintf(`{"nodeId":%q,"keys":["abc"],"next":""}`, node),
		"duplicate keys":    fmt.Sprintf(`{"nodeId":%q,"keys":[%q,%q,%q],"next":""}`, node, k1, k2, k1),
		"next not last key": fmt.Sprintf(`{"nodeId":%q,"keys":[%q,%q],"next":%q}`, node, k1, k2, k1),
		"next without keys": fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":%q}`, node, k1),
		"too many keys":     fmt.Sprintf(`{"nodeId":%q,"keys":[%s],"next":""}`, node, strings.Join(tooMany, ",")),
		"oversized":         fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":""}`, node) + strings.Repeat(" ", maxKeysPageBytes),
	}
	for name, encoded := range tests {
		if _, err := decodeKeysPage([]byte(encoded)); err == nil {
			t.Errorf("%s: decode succeeded", name)
		}
	}
}
