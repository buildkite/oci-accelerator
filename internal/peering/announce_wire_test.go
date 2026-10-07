package peering

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAnnouncementRoundTrip(t *testing.T) {
	key, _ := parseCacheKey(testKeyText)
	message := announcement{
		sender:    mustNodeID(t, testIDB),
		recipient: mustNodeID(t, testIDC),
		hints: []announcedHint{
			testHint(t, testIDA, key, testIDA, testIDB),
			testHint(t, testIDB, testCacheKey(2), testIDB),
		},
	}
	encoded, err := encodeAnnouncement(message)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[` +
		`{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]},` +
		`{"providerNodeId":"nodeB-ID_456","key":"` + testCacheKey(2).String() + `","seen":["nodeB-ID_456"]}]}`
	if string(encoded) != want {
		t.Fatalf("encoded\n%s\nwant\n%s", encoded, want)
	}
	decoded, err := decodeAnnouncement(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, message) {
		t.Fatalf("decoded %+v, want %+v", decoded, message)
	}
}

func TestDecodeAnnouncementRejectsMalformedMessages(t *testing.T) {
	wireHint := `{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`
	message := func(hints string) string {
		return `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[` + hints + `]}`
	}
	withSeen := func(seen string) string {
		return message(`{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":` + seen + `}`)
	}
	if _, err := decodeAnnouncement([]byte(message(wireHint))); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	tooMany := strings.TrimSuffix(strings.Repeat(hintFor(0)+",", maxAnnouncementHints+1), ",")
	cases := map[string]string{
		"empty body":                 ``,
		"not an object":              `[]`,
		"trailing data":              message(wireHint) + `{}`,
		"unknown field":              `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[` + wireHint + `],"extra":1}`,
		"duplicate field":            `{"senderNodeId":"nodeB-ID_456","senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[` + wireHint + `]}`,
		"missing recipient":          `{"senderNodeId":"nodeB-ID_456","hints":[` + wireHint + `]}`,
		"wrong field case":           `{"SenderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[` + wireHint + `]}`,
		"null hints":                 `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":null}`,
		"hints object":               `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":{}}`,
		"numeric sender":             `{"senderNodeId":1,"recipientNodeId":"nodeC-ID_789","hints":[` + wireHint + `]}`,
		"empty hints":                message(``),
		"too many hints":             message(tooMany),
		"invalid sender":             `{"senderNodeId":"short","recipientNodeId":"nodeC-ID_789","hints":[` + wireHint + `]}`,
		"sender is recipient":        `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeB-ID_456","hints":[` + wireHint + `]}`,
		"hint unknown field":         message(`{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"],"x":1}`),
		"hint missing key":           message(`{"providerNodeId":"nodeA-ID_123","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"hint duplicate field":       message(`{"providerNodeId":"nodeA-ID_123","providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"hint wrong case":            message(`{"ProviderNodeID":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"uppercase key":              message(`{"providerNodeId":"nodeA-ID_123","key":"` + strings.ToUpper(testKeyText) + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"short key":                  message(`{"providerNodeId":"nodeA-ID_123","key":"abcd","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"invalid provider":           message(`{"providerNodeId":"nodeA-ID_12!","key":"` + testKeyText + `","seen":["nodeA-ID_123","nodeB-ID_456"]}`),
		"null seen":                  withSeen(`null`),
		"numeric seen ID":            withSeen(`["nodeA-ID_123",1]`),
		"invalid seen ID":            withSeen(`["nodeA-ID_123","nodeB-ID_456","bad"]`),
		"unsorted seen":              withSeen(`["nodeB-ID_456","nodeA-ID_123"]`),
		"duplicate seen":             withSeen(`["nodeA-ID_123","nodeA-ID_123","nodeB-ID_456"]`),
		"seen missing provider":      withSeen(`["nodeB-ID_456"]`),
		"seen missing sender":        withSeen(`["nodeA-ID_123"]`),
		"seen too large":             withSeen(largeSeen(maxHintSeen + 1)),
		"duplicate provider and key": message(wireHint + "," + wireHint),
		"oversized body":             message(wireHint) + strings.Repeat(" ", maxAnnouncementBytes),
	}
	for name, body := range cases {
		if _, err := decodeAnnouncement([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeAnnouncementAcceptsMaximumSeen(t *testing.T) {
	seen := largeSeen(maxHintSeen)
	body := `{"senderNodeId":"nodeB-ID_456","recipientNodeId":"nodeC-ID_789","hints":[{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":` + seen + `}]}`
	message, err := decodeAnnouncement([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(message.hints[0].seen); got != maxHintSeen {
		t.Fatalf("seen = %d", got)
	}
}

func TestEncodeAnnouncementRejectsInvalidMessages(t *testing.T) {
	sender, recipient := mustNodeID(t, testIDB), mustNodeID(t, testIDC)
	valid := testHint(t, testIDA, testCacheKey(1), testIDA, testIDB)
	many := make([]announcedHint, maxAnnouncementHints+1)
	for i := range many {
		many[i] = testHint(t, testIDA, testCacheKey(i), testIDA, testIDB)
	}
	// Few enough hints, but too many bytes.
	var heavy []announcedHint
	for i := 0; i < 40; i++ {
		ids := []string{testIDA, testIDB}
		for j := range maxHintSeen - 2 {
			ids = append(ids, testNodeID(j))
		}
		heavy = append(heavy, testHint(t, testIDA, testCacheKey(i), ids...))
	}
	cases := map[string]announcement{
		"no hints":            {sender: sender, recipient: recipient},
		"sender is recipient": {sender: sender, recipient: sender, hints: []announcedHint{valid}},
		"seen missing sender": {sender: sender, recipient: recipient, hints: []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA)}},
		"unsorted seen":       {sender: sender, recipient: recipient, hints: []announcedHint{{provider: valid.provider, key: valid.key, seen: []nodeID{valid.seen[1], valid.seen[0]}}}},
		"duplicate pair":      {sender: sender, recipient: recipient, hints: []announcedHint{valid, valid}},
		"zero recipient":      {sender: sender, hints: []announcedHint{valid}},
		"zero seen ID":        {sender: sender, recipient: recipient, hints: []announcedHint{{provider: valid.provider, key: valid.key, seen: append([]nodeID{{}}, valid.seen...)}}},
		"too many hints":      {sender: sender, recipient: recipient, hints: many},
		"oversized":           {sender: sender, recipient: recipient, hints: heavy},
	}
	for name, message := range cases {
		if _, err := encodeAnnouncement(message); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

// largeSeen returns a sorted JSON seen array of n IDs containing the test
// provider and sender.
func largeSeen(n int) string {
	ids := make([]string, 0, n)
	for i := range n - 2 {
		ids = append(ids, testNodeID(i))
	}
	ids = append(ids, testIDA, testIDB)
	encoded, _ := json.Marshal(ids)
	return string(encoded)
}
