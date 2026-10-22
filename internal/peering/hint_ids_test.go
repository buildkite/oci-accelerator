package peering

import (
	"strings"
	"testing"
)

func TestParseCacheKey(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	key, ok := parseCacheKey(valid)
	if !ok {
		t.Fatalf("parseCacheKey(%q) rejected a valid key", valid)
	}
	if key[0] != 0x01 || key[31] != 0xef {
		t.Fatalf("decoded key = %x", key)
	}
	if got := key.String(); got != valid {
		t.Fatalf("String() = %q, want %q", got, valid)
	}

	for name, s := range map[string]string{
		"empty":     "",
		"uppercase": strings.Repeat("0123456789ABCDEF", 4),
		"short":     valid[:63],
		"long":      valid + "0",
		"non-hex":   valid[:63] + "g",
		"space":     " " + valid[1:],
		"multibyte": valid[:62] + "é",
	} {
		if _, ok := parseCacheKey(s); ok {
			t.Errorf("%s: parseCacheKey(%q) accepted an invalid key", name, s)
		}
	}
}

func TestParseNodeID(t *testing.T) {
	for _, s := range []string{testNodeID(7), "AZaz09-_AZaz"} {
		id, ok := parseNodeID(s)
		if !ok {
			t.Fatalf("parseNodeID(%q) rejected a valid ID", s)
		}
		if got := id.String(); got != s {
			t.Fatalf("String() = %q, want %q", got, s)
		}
	}
	for _, s := range []string{"", "short", testNodeID(1) + "x", "abc/defghijk", "abcdefghijk="} {
		if _, ok := parseNodeID(s); ok {
			t.Errorf("parseNodeID(%q) accepted an invalid ID", s)
		}
	}
}
