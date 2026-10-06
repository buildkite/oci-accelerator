package peering

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAvailabilityRoundTrip(t *testing.T) {
	key, _ := parseCacheKey(testKeyText)
	message := availabilityMessage{
		sender:    mustNodeID(t, testIDB),
		recipient: mustNodeID(t, testIDC),
		claims: []hintClaim{
			testClaim(t, testIDA, key, testIDA, testIDB),
			testClaim(t, testIDB, testCacheKey(2), testIDB),
		},
	}
	encoded, err := encodeAvailability(message)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[` +
		`{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]},` +
		`{"ownerNodeId":"nodeB-ID_456","key":"` + testCacheKey(2).String() + `","seen":["nodeB-ID_456"]}]}`
	if string(encoded) != want {
		t.Fatalf("encoded\n%s\nwant\n%s", encoded, want)
	}
	decoded, err := decodeAvailability(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, message) {
		t.Fatalf("decoded %+v, want %+v", decoded, message)
	}
}

func TestDecodeAvailabilityRejectsMalformedMessages(t *testing.T) {
	claim := `{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`
	message := func(claims string) string {
		return `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[` + claims + `]}`
	}
	withSeen := func(seen string) string {
		return message(`{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":` + seen + `}`)
	}
	if _, err := decodeAvailability([]byte(message(claim))); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	tooMany := strings.TrimSuffix(strings.Repeat(claimFor(0)+",", maxAvailabilityClaim+1), ",")
	cases := map[string]string{
		"empty body":              ``,
		"not an object":           `[]`,
		"trailing data":           message(claim) + `{}`,
		"unknown field":           `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[` + claim + `],"extra":1}`,
		"duplicate field":         `{"senderNodeId":"nodeB-ID_456","senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[` + claim + `]}`,
		"missing recipient":       `{"senderNodeId":"nodeB-ID_456","claims":[` + claim + `]}`,
		"wrong field case":        `{"SenderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[` + claim + `]}`,
		"null claims":             `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":null}`,
		"claims object":           `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":{}}`,
		"numeric sender":          `{"senderNodeId":1,"recipientNodeId":"nodeC-ID_789","claims":[` + claim + `]}`,
		"empty claims":            message(``),
		"too many claims":         message(tooMany),
		"invalid sender":          `{"senderNodeId":"short","recipientNodeId":"nodeC-ID_789","claims":[` + claim + `]}`,
		"sender is recipient":     `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeB-ID_456","claims":[` + claim + `]}`,
		"claim unknown field":     message(`{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"],"x":1}`),
		"claim missing key":       message(`{"ownerNodeId":"nodeA-ID_123","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"claim duplicate field":   message(`{"ownerNodeId":"nodeA-ID_123","ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"claim wrong case":        message(`{"OwnerNodeID":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"uppercase key":           message(`{"ownerNodeId":"nodeA-ID_123","key":"` + strings.ToUpper(testKeyText) + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"short key":               message(`{"ownerNodeId":"nodeA-ID_123","key":"abcd","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"invalid owner":           message(`{"ownerNodeId":"nodeA-ID_12!","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"null seen":               withSeen(`null`),
		"numeric seen ID":         withSeen(`["nodeA-ID_123",1]`),
		"invalid seen ID":         withSeen(`["nodeA-ID_123","nodeB-ID_456","bad"]`),
		"unsorted seen":           withSeen(`["nodeB-ID_456","nodeA-ID_123"]`),
		"duplicate seen":          withSeen(`["nodeA-ID_123","nodeA-ID_123","nodeB-ID_456"]`),
		"seen missing owner":      withSeen(`["nodeB-ID_456"]`),
		"seen missing sender":     withSeen(`["nodeA-ID_123"]`),
		"seen too large":          withSeen(largeSeen(maxHintSeen + 1)),
		"duplicate owner and key": message(claim + "," + claim),
		"oversized body":          message(claim) + strings.Repeat(" ", maxAvailabilityBytes),
	}
	for name, body := range cases {
		if _, err := decodeAvailability([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeAvailabilityAcceptsMaximumSeen(t *testing.T) {
	seen := largeSeen(maxHintSeen)
	body := `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","claims":[{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":` + seen + `}]}`
	message, err := decodeAvailability([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(message.claims[0].seen); got != maxHintSeen {
		t.Fatalf("seen = %d", got)
	}
}

func TestEncodeAvailabilityRejectsInvalidMessages(t *testing.T) {
	sender, recipient := mustNodeID(t, testIDB), mustNodeID(t, testIDC)
	valid := testClaim(t, testIDA, testCacheKey(1), testIDA, testIDB)
	many := make([]hintClaim, maxAvailabilityClaim+1)
	for i := range many {
		many[i] = testClaim(t, testIDA, testCacheKey(i), testIDA, testIDB)
	}
	// Few enough claims, but too many bytes.
	var heavy []hintClaim
	for i := 0; i < 40; i++ {
		ids := []string{testIDA, testIDB}
		for j := range maxHintSeen - 2 {
			ids = append(ids, testNodeID(j))
		}
		heavy = append(heavy, testClaim(t, testIDA, testCacheKey(i), ids...))
	}
	cases := map[string]availabilityMessage{
		"no claims":           {sender: sender, recipient: recipient},
		"sender is recipient": {sender: sender, recipient: sender, claims: []hintClaim{valid}},
		"seen missing sender": {sender: sender, recipient: recipient, claims: []hintClaim{testClaim(t, testIDA, testCacheKey(1), testIDA)}},
		"unsorted seen":       {sender: sender, recipient: recipient, claims: []hintClaim{{owner: valid.owner, key: valid.key, seen: []nodeID{valid.seen[1], valid.seen[0]}}}},
		"duplicate pair":      {sender: sender, recipient: recipient, claims: []hintClaim{valid, valid}},
		"zero recipient":      {sender: sender, claims: []hintClaim{valid}},
		"zero seen ID":        {sender: sender, recipient: recipient, claims: []hintClaim{{owner: valid.owner, key: valid.key, seen: append([]nodeID{{}}, valid.seen...)}}},
		"too many claims":     {sender: sender, recipient: recipient, claims: many},
		"oversized":           {sender: sender, recipient: recipient, claims: heavy},
	}
	for name, message := range cases {
		if _, err := encodeAvailability(message); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

// largeSeen returns a sorted JSON seen array of n IDs containing the test
// owner and sender.
func largeSeen(n int) string {
	ids := make([]string, 0, n)
	for i := range n - 2 {
		ids = append(ids, testNodeID(i))
	}
	ids = append(ids, testIDA, testIDB)
	encoded, _ := json.Marshal(ids)
	return string(encoded)
}
