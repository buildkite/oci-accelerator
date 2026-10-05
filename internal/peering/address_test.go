package peering

import "testing"

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
