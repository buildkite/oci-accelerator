package config

import (
	"errors"
	"testing"
)

func TestMetricsListenValidation(t *testing.T) {
	valid := []string{"127.0.0.1:9090", "[::1]:9090"}
	for _, address := range valid {
		c, err := Load(writeConfig(t, validConfig+"metrics_listen: '"+address+"'\n"))
		if err != nil || c.MetricsListen != address {
			t.Errorf("valid %q: %+v %v", address, c, err)
		}
	}
	invalid := []struct{ name, address string }{
		{"hostname", "localhost:9090"}, {"unspecified_ipv4", "0.0.0.0:9090"},
		{"absolute_hostname", "localhost.:9090"},
		{"non_loopback_ipv4", "192.0.2.1:9090"}, {"zero_port", "127.0.0.1:0"},
		{"named_port", "127.0.0.1:http"}, {"scoped_ipv6", "[fe80::1%lo]:9090"},
		{"missing_host", ":9090"},
	}
	for _, test := range invalid {
		t.Run("invalid_"+test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, validConfig+"metrics_listen: '"+test.address+"'\n")); !errors.Is(err, ErrInvalid) {
				t.Errorf("invalid %q: %v", test.address, err)
			}
		})
	}
}

func TestLoadCanonicalizesListen(t *testing.T) {
	tests := map[string]string{
		"EXAMPLE.com.:3128":   "example.com.:3128",
		"EXAMPLE.com.:03128":  "example.com.:3128",
		"EXAMPLE.com:3128":    "example.com:3128",
		"LOCALHOST.:3128":     "localhost.:3128",
		"[2001:0DB8::1]:3128": "[2001:db8::1]:3128",
		"127.0.0.1:03128":     "127.0.0.1:3128",
		":3128":               ":3128",
		"0.0.0.0:3128":        "0.0.0.0:3128",
		"[::]:3128":           "[::]:3128",
		"127.0.0.1:3128":      "127.0.0.1:3128",
	}
	for input, want := range tests {
		c, err := Load(writeConfig(t, validConfig+"listen: '"+input+"'\n"))
		if err != nil {
			t.Errorf("Load(listen=%q) returned %v", input, err)
			continue
		}
		if c.Listen != want {
			t.Errorf("Load(listen=%q) = %q, want %q", input, c.Listen, want)
		}
	}
}

func TestLoadRejectsInvalidListen(t *testing.T) {
	invalid := []struct{ name, input string }{
		{"underscore_in_host", "Host_Underscore:3128"},
		{"empty_dns_label", "a..b:3128"},
		{"multiple_trailing_dots", "example.com..:3128"},
		{"space_in_host", "bad host:3128"},
		{"port_out_of_range", "127.0.0.1:99999"},
		{"missing_port", "127.0.0.1:"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, validConfig+"listen: '"+test.input+"'\n")); !errors.Is(err, ErrInvalid) {
				t.Errorf("Load(listen=%q) unexpectedly returned %v", test.input, err)
			}
		})
	}
}

func TestLoadCanonicalizesMetricsListen(t *testing.T) {
	c, err := Load(writeConfig(t, validConfig+"metrics_listen: '127.0.0.1:09090'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsListen != "127.0.0.1:9090" {
		t.Fatalf("metrics_listen = %q, want 127.0.0.1:9090", c.MetricsListen)
	}
}
