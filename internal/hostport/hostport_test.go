package hostport

import "testing"

func TestParseCanonicalizes(t *testing.T) {
	tests := map[string]string{
		"EXAMPLE.com.":       "example.com",
		"example.com:443":    "example.com",
		"example.com:0443":   "example.com",
		"example.com:5000":   "example.com:5000",
		"[2001:0DB8::1]:443": "[2001:db8::1]",
		"2001:db8::1":        "[2001:db8::1]",
		"127.0.0.1":          "127.0.0.1",
	}
	for input, want := range tests {
		a, err := Parse(input, "443")
		if err != nil || a.String() != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", input, a.String(), err, want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	inputs := []string{"", " host", "user@host", "host/path", "host?x", "host#x", "*.host", "fe80::1%eth0", "host:0", "host:65536", "-host", "host..name", "[::1", "127.000.000.001", "host:", "[example.com]:443", "[::1]:"}
	for _, input := range inputs {
		if _, err := Parse(input, "443"); err == nil {
			t.Errorf("Parse(%q) succeeded", input)
		}
	}
}

func TestParseRequiresCONNECTPort(t *testing.T) {
	if _, err := Parse("example.com", ""); err == nil {
		t.Fatal("authority without CONNECT port accepted")
	}
	a, err := Parse("example.com:443", "")
	if err != nil || a.String() != "example.com" {
		t.Fatalf("explicit CONNECT authority: %v, %q", err, a.String())
	}
}
