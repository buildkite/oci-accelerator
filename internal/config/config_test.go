package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertEqual[T comparable](t *testing.T, field string, actual, expected T) {
	t.Helper()
	if actual != expected {
		t.Errorf("%s = %v, want %v", field, actual, expected)
	}
}

func TestLoadDefaultsAndRelativePaths(t *testing.T) {
	path := writeConfig(t, "registries: [EXAMPLE.com.:443, '[::1]:5000']\nca_dir: ca\ncache:\n  dir: cache\n  max_bytes: null\ndial_timeout: null\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	assertEqual(t, "Listen", c.Listen, "127.0.0.1:3128")
	assertEqual(t, "CADir", c.CADir, filepath.Join(base, "ca"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(base, "cache"))
	assertEqual(t, "Cache.MaxBytes", c.Cache.MaxBytes, int64(DefaultCacheMaxBytes))
	assertEqual(t, "Cache.MaxFills", c.Cache.MaxFills, DefaultCacheMaxFills)
	assertEqual(t, "Cache.MaxEntries", c.Cache.MaxEntries, DefaultCacheMaxEntries)
	assertEqual(t, "Cache.FillTimeout", c.Cache.FillTimeout, DefaultCacheFillTimeout)
	assertEqual(t, "Cache.FillWaitTimeout", c.Cache.FillWaitTimeout, time.Second)
	assertEqual(t, "DialTimeout", c.DialTimeout, 10*time.Second)
	assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 10*time.Second)
	assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 30*time.Second)
	assertEqual(t, "IdleTimeout", c.IdleTimeout, 2*time.Minute)
	assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 15*time.Second)
	assertEqual(t, "MaxConnections", c.MaxConnections, 256)
	assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 256)
	if c.Registries[0].String() != "example.com" || c.Registries[1].String() != "[::1]:5000" {
		t.Fatalf("registries: %v", c.Registries)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist should default empty: %v", c.RedirectAllowlist)
	}
}

func TestLoadExplicitOperationalOverrides(t *testing.T) {
	path := writeConfig(t, "listen: ':8080'\nregistries: [example.com]\nca_dir: /ca\ncache: {dir: /cache, max_bytes: 12345, max_fills: 7, max_entries: 99, fill_timeout: 3m, fill_wait_timeout: 0s}\ndial_timeout: 1s\ntls_handshake_timeout: 2s\nresponse_header_timeout: 3s\nidle_timeout: 4s\nshutdown_timeout: 5s\nmax_connections: 6\nmax_leaf_certificates: 8\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "Listen", c.Listen, ":8080")
	assertEqual(t, "CADir", c.CADir, "/ca")
	assertEqual(t, "Cache.Dir", c.Cache.Dir, "/cache")
	assertEqual(t, "Cache.MaxBytes", c.Cache.MaxBytes, int64(12345))
	assertEqual(t, "Cache.MaxFills", c.Cache.MaxFills, 7)
	assertEqual(t, "Cache.MaxEntries", c.Cache.MaxEntries, 99)
	assertEqual(t, "Cache.FillTimeout", c.Cache.FillTimeout, 3*time.Minute)
	assertEqual(t, "Cache.FillWaitTimeout", c.Cache.FillWaitTimeout, time.Duration(0))
	assertEqual(t, "DialTimeout", c.DialTimeout, time.Second)
	assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 2*time.Second)
	assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 3*time.Second)
	assertEqual(t, "IdleTimeout", c.IdleTimeout, 4*time.Second)
	assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 5*time.Second)
	assertEqual(t, "MaxConnections", c.MaxConnections, 6)
	assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 8)
}

func TestLoadRedirectAllowlist(t *testing.T) {
	path := writeConfig(t, "registries: [registry.example]\nredirect_allowlist: [CDN.EXAMPLE.:443, cdn.example:8443, '[2001:0DB8::1]', '[::1]:5000']\nca_dir: ca\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cdn.example", "cdn.example:8443", "[2001:db8::1]", "[::1]:5000"}
	if len(c.RedirectAllowlist) != len(want) {
		t.Fatalf("redirect allowlist: %v", c.RedirectAllowlist)
	}
	for i, authority := range c.RedirectAllowlist {
		if authority.String() != want[i] {
			t.Errorf("redirect allowlist[%d] = %q, want %q", i, authority.String(), want[i])
		}
	}
	if len(c.Registries) != 1 || c.Registries[0].String() != "registry.example" {
		t.Fatalf("redirect allowlist changed registries: %v", c.Registries)
	}
}

func TestLoadAllowsEmptyRedirectAllowlist(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [registry.example]\nredirect_allowlist: []\nca_dir: ca\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist: %v", c.RedirectAllowlist)
	}
}

func TestMetricsListenValidation(t *testing.T) {
	valid := []string{"127.0.0.1:9090", "[::1]:9090"}
	for _, address := range valid {
		c, err := Load(writeConfig(t, "registries: [registry.example]\nca_dir: ca\nmetrics_listen: '"+address+"'\n"))
		if err != nil || c.MetricsListen != address {
			t.Errorf("valid %q: %+v %v", address, c, err)
		}
	}
	invalid := []struct{ name, address string }{
		{"hostname", "localhost:9090"}, {"unspecified_ipv4", "0.0.0.0:9090"},
		{"non_loopback_ipv4", "192.0.2.1:9090"}, {"zero_port", "127.0.0.1:0"},
		{"named_port", "127.0.0.1:http"}, {"scoped_ipv6", "[fe80::1%lo]:9090"},
		{"missing_host", ":9090"},
	}
	for _, test := range invalid {
		t.Run("invalid_"+test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, "registries: [registry.example]\nca_dir: ca\nmetrics_listen: '"+test.address+"'\n")); !errors.Is(err, ErrInvalid) {
				t.Errorf("invalid %q: %v", test.address, err)
			}
		})
	}
}

func TestLoadRejectsInvalidRedirectAllowlist(t *testing.T) {
	tests := []struct{ name, entry string }{
		{"credentials", "redirect_allowlist: [user:secret@cdn.example]"},
		{"zero_port", "redirect_allowlist: [cdn.example:0]"},
		{"port_out_of_range", "redirect_allowlist: [cdn.example:99999]"},
		{"leading_space", "redirect_allowlist: [' cdn.example']"},
		{"duplicate_default_port", "redirect_allowlist: [cdn.example, CDN.EXAMPLE.:443]"},
		{"duplicate_padded_port", "redirect_allowlist: [cdn.example:8443, CDN.EXAMPLE.:08443]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "registries: [registry.example]\nca_dir: ca\n" + test.entry + "\n"
			_, err := Load(writeConfig(t, input))
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Load(%q) unexpectedly returned %v", test.entry, err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "cdn.example")) {
				t.Errorf("error leaked redirect allowlist input: %v", err)
			}
		})
	}
}

func TestLoadRejectsNullListElements(t *testing.T) {
	tests := []struct{ name, entry string }{
		{"registry_trailing_null", "registries: [example.com, null]"},
		{"registry_null", "registries: [null]"},
		{"registry_empty", "registries: ['']"},
		{"redirect_trailing_null", "registries: [example.com]\nredirect_allowlist: [cdn.example, null]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "ca_dir: ca\n" + test.entry + "\n"
			_, err := Load(writeConfig(t, input))
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Load(%q) unexpectedly returned %v", test.entry, err)
			}
			if err != nil && (strings.Contains(err.Error(), "cdn.example") || strings.Contains(err.Error(), "example.com")) {
				t.Errorf("error leaked list input: %v", err)
			}
		})
	}
}

func TestLoadCanonicalizesListen(t *testing.T) {
	tests := map[string]string{
		"EXAMPLE.com.:3128":   "example.com:3128",
		"[2001:0DB8::1]:3128": "[2001:db8::1]:3128",
		"127.0.0.1:03128":     "127.0.0.1:3128",
		":3128":               ":3128",
		"0.0.0.0:3128":        "0.0.0.0:3128",
		"[::]:3128":           "[::]:3128",
		"127.0.0.1:3128":      "127.0.0.1:3128",
	}
	for input, want := range tests {
		c, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\nlisten: '"+input+"'\n"))
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
		{"space_in_host", "bad host:3128"},
		{"port_out_of_range", "127.0.0.1:99999"},
		{"missing_port", "127.0.0.1:"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\nlisten: '"+test.input+"'\n")); !errors.Is(err, ErrInvalid) {
				t.Errorf("Load(listen=%q) unexpectedly returned %v", test.input, err)
			}
		})
	}
}

func TestLoadCanonicalizesMetricsListen(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\nmetrics_listen: '127.0.0.1:09090'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsListen != "127.0.0.1:9090" {
		t.Fatalf("metrics_listen = %q, want 127.0.0.1:9090", c.MetricsListen)
	}
}

func TestLoadRejectsNonIntegerNumericLimits(t *testing.T) {
	tests := []struct{ name, entry string }{
		{"fractional_max_bytes", "cache: {max_bytes: 1.9}"},
		{"float_max_bytes", "cache: {max_bytes: 1.0}"},
		{"string_max_fills", "cache: {max_fills: \"7\"}"},
		{"fractional_max_fills", "cache: {max_fills: 1.5}"},
		{"fractional_max_entries", "cache: {max_entries: 1.5}"},
		{"boolean_max_connections", "max_connections: true"},
		{"fractional_max_connections", "max_connections: 1.5"},
		{"fractional_max_leaf_certificates", "max_leaf_certificates: 1.5"},
		// Non-decimal integer forms are !!int but not decimal; rejected for clarity.
		{"hex_max_bytes", "cache: {max_bytes: 0xff}"},
		{"octal_max_bytes", "cache: {max_bytes: 0o17}"},
		{"legacy_octal_max_bytes", "cache: {max_bytes: 017}"},
		{"underscored_max_bytes", "cache: {max_bytes: 1_000}"},
		// A non-decimal form on a strictInt field exercises the shared parse path.
		{"octal_max_connections", "max_connections: 0o17"},
		// A genuinely !!int value above MaxInt64 hits the decimal ParseInt range
		// error. A 20+ digit literal instead resolves to !!float and is rejected
		// earlier by the tag guard, so it would not cover this branch.
		{"max_bytes_out_of_range", "cache: {max_bytes: 9223372036854775808}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "registries: [example.com]\nca_dir: ca\n" + test.entry + "\n"
			if _, err := Load(writeConfig(t, input)); !errors.Is(err, ErrInvalid) {
				t.Errorf("Load(%q) unexpectedly returned %v", test.entry, err)
			}
		})
	}
}

func TestLoadPreservesDefaultForNullNumericLimit(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\ncache: {max_fills: null}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxFills != DefaultCacheMaxFills {
		t.Fatalf("max_fills null should preserve default: %d", c.Cache.MaxFills)
	}
}

func TestLoadAcceptsLargeValidInteger(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\ncache: {max_bytes: 34359738368}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxBytes != 34359738368 {
		t.Fatalf("max_bytes: %d", c.Cache.MaxBytes)
	}
}

func TestLoadAcceptsSignedInteger(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [example.com]\nca_dir: ca\ncache: {max_fills: +7}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxFills != 7 {
		t.Fatalf("max_fills +7 should load as 7: %d", c.Cache.MaxFills)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := []struct{ name, input string }{
		{"missing_registries", "ca_dir: ca\n"},
		{"missing_ca_dir", "registries: [example.com]\n"},
		{"unknown_field", "registries: [example.com]\nca_dir: ca\nunknown: true\n"},
		{"duplicate_registries_key", "registries: [example.com]\nregistries: [other.example]\nca_dir: ca\n"},
		{"duplicate_registry", "registries: [example.com, EXAMPLE.COM:443]\nca_dir: ca\n"},
		{"registry_credentials", "registries: [user:pass@example.com]\nca_dir: ca\n"},
		{"empty_registry", "registries: ['']\nca_dir: ca\n"},
		{"zero_dial_timeout", "registries: [example.com]\nca_dir: ca\ndial_timeout: 0s\n"},
		{"zero_max_connections", "registries: [example.com]\nca_dir: ca\nmax_connections: 0\n"},
		{"zero_max_fills", "registries: [example.com]\nca_dir: ca\ncache: {max_fills: 0}\n"},
		{"negative_max_entries", "registries: [example.com]\nca_dir: ca\ncache: {max_entries: -1}\n"},
		{"zero_fill_timeout", "registries: [example.com]\nca_dir: ca\ncache: {fill_timeout: 0s}\n"},
		{"negative_fill_wait_timeout", "registries: [example.com]\nca_dir: ca\ncache: {fill_wait_timeout: -1s}\n"},
		{"listen_port_out_of_range", "registries: [example.com]\nca_dir: ca\nlisten: '127.0.0.1:99999'\n"},
		{"listen_missing_port", "registries: [example.com]\nca_dir: ca\nlisten: '127.0.0.1:'\n"},
		{"listen_invalid_host", "registries: [example.com]\nca_dir: ca\nlisten: 'bad host:3128'\n"},
		{"multiple_documents", "registries: [example.com]\nca_dir: ca\n---\n{}\n"},
		{"malformed_yaml", "registries: [example.com\nca_dir: secret-value\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.input))
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Load unexpectedly returned %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "user:pass")) {
				t.Errorf("error leaked input: %v", err)
			}
		})
	}
}
