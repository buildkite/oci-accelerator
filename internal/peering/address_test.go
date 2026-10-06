package peering

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCanonicalAddress(t *testing.T) {
	tests := map[string]string{
		"EXAMPLE.COM.:0443":  "example.com:443",
		"127.0.0.1:443":      "127.0.0.1:443",
		"[2001:0DB8::1]:443": "[2001:db8::1]:443",
	}
	for input, want := range tests {
		got, err := CanonicalAddress(input)
		if err != nil || got != want {
			t.Errorf("CanonicalAddress(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
}

func TestCanonicalAddressRejectsUnusableAddresses(t *testing.T) {
	for _, input := range []string{"example.com", ":443", "*:443", "0.0.0.0:443", "[::]:443", "224.0.0.1:443", "[ff02::1]:443"} {
		if got, err := CanonicalAddress(input); err == nil {
			t.Errorf("CanonicalAddress(%q) = %q, want error", input, got)
		}
	}
}

func TestCanonicalMessage(t *testing.T) {
	got, err := canonicalMessage(Message{
		Address: "SELF.TEST.:0443",
		NodeID:  "MixedCase-Id",
		Peers: map[string]string{
			"A.TEST:1":         testIDA,
			"a.test.:01":       testIDA,
			"[2001:0DB8::1]:2": "mixedcase-id",
		},
	})
	want := Message{
		Address: "self.test:443",
		NodeID:  "MixedCase-Id",
		Peers:   map[string]string{"a.test:1": testIDA, "[2001:db8::1]:2": "mixedcase-id"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalMessage() = %#v, %v, want %#v", got, err, want)
	}
}

func TestCanonicalMessageNilPeersBecomeEmpty(t *testing.T) {
	got, err := canonicalMessage(Message{Address: "self.test:443", NodeID: testIDA})
	if err != nil || got.Peers == nil || len(got.Peers) != 0 {
		t.Fatalf("canonicalMessage() = %#v, %v", got, err)
	}
}

func TestCanonicalMessageAllowsContradictoryClaimsAboutSender(t *testing.T) {
	message := Message{
		Address: "self.test:443",
		NodeID:  testIDA,
		Peers:   map[string]string{"SELF.TEST:443": testIDB, "other.test:1": testIDA},
	}
	got, err := canonicalMessage(message)
	if err != nil || got.NodeID != testIDA || got.Peers["self.test:443"] != testIDB {
		t.Fatalf("canonicalMessage() = %#v, %v", got, err)
	}
}

func TestCanonicalMessageRejectsInvalidRecords(t *testing.T) {
	tooMany := make(map[string]string, MaxPeers+1)
	for i := range MaxPeers + 1 {
		tooMany[fmt.Sprintf("p%04d.test:1", i)] = testIDB
	}
	tests := map[string]Message{
		"address":            {Address: "self.test", NodeID: testIDA},
		"missing node ID":    {Address: "self.test:443"},
		"short node ID":      {Address: "self.test:443", NodeID: "short"},
		"long node ID":       {Address: "self.test:443", NodeID: testIDA + "x"},
		"padded node ID":     {Address: "self.test:443", NodeID: "nodeA-ID_12="},
		"standard base64 ID": {Address: "self.test:443", NodeID: "nodeA+ID/123"},
		"peer address":       {Address: "self.test:443", NodeID: testIDA, Peers: map[string]string{"bad": testIDB}},
		"peer node ID":       {Address: "self.test:443", NodeID: testIDA, Peers: map[string]string{"b.test:1": "bad"}},
		"conflicting alias":  {Address: "self.test:443", NodeID: testIDA, Peers: map[string]string{"B.TEST:1": testIDB, "b.test.:01": testIDC}},
		"case-variant alias": {Address: "self.test:443", NodeID: testIDA, Peers: map[string]string{"B.TEST:1": "abcdefghijkl", "b.test:1": "ABCDEFGHIJKL"}},
		"too many peers":     {Address: "self.test:443", NodeID: testIDA, Peers: tooMany},
	}
	for name, message := range tests {
		if got, err := canonicalMessage(message); err == nil {
			t.Errorf("%s: canonicalMessage() = %#v, want error", name, got)
		}
	}
}

func TestCanonicalMessageAcceptsMaxPeers(t *testing.T) {
	peers := make(map[string]string, MaxPeers)
	for i := range MaxPeers {
		peers[fmt.Sprintf("p%04d.test:1", i)] = testIDB
	}
	got, err := canonicalMessage(Message{Address: "self.test:443", NodeID: testIDA, Peers: peers})
	if err != nil || len(got.Peers) != MaxPeers {
		t.Fatalf("canonicalMessage() peers = %d, %v", len(got.Peers), err)
	}
}
