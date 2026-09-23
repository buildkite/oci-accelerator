package config

import (
	"errors"
	"testing"
)

func TestLoadRejectsNonIntegerNumericLimits(t *testing.T) {
	tests := []struct{ name, entry string }{
		{"fractional_max_bytes", "cache: {dir: cache, max_bytes: 1.9}"},
		{"float_max_bytes", "cache: {dir: cache, max_bytes: 1.0}"},
		{"string_max_fills", "cache: {dir: cache, max_fills: \"7\"}"},
		{"fractional_max_fills", "cache: {dir: cache, max_fills: 1.5}"},
		{"fractional_max_entries", "cache: {dir: cache, max_entries: 1.5}"},
		{"string_max_authorization_entries", "cache: {dir: cache, max_authorization_entries: \"100\"}"},
		{"fractional_max_authorization_entries", "cache: {dir: cache, max_authorization_entries: 1.5}"},
		{"boolean_max_connections", "cache: {dir: cache}\nmax_connections: true"},
		{"fractional_max_connections", "cache: {dir: cache}\nmax_connections: 1.5"},
		{"fractional_max_leaf_certificates", "cache: {dir: cache}\nmax_leaf_certificates: 1.5"},
		// Non-decimal integer forms are !!int but not decimal; rejected for clarity.
		{"hex_max_bytes", "cache: {dir: cache, max_bytes: 0xff}"},
		{"octal_max_bytes", "cache: {dir: cache, max_bytes: 0o17}"},
		{"legacy_octal_max_bytes", "cache: {dir: cache, max_bytes: 017}"},
		{"underscored_max_bytes", "cache: {dir: cache, max_bytes: 1_000}"},
		// A non-decimal form on a strictInt field exercises the shared parse path.
		{"octal_max_connections", "cache: {dir: cache}\nmax_connections: 0o17"},
		// A genuinely !!int value above MaxInt64 hits the decimal ParseInt range
		// error. A 20+ digit literal instead resolves to !!float and is rejected
		// earlier by the tag guard, so it would not cover this branch.
		{"max_bytes_out_of_range", "cache: {dir: cache, max_bytes: 9223372036854775808}"},
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

func TestLoadRejectsNonPositiveMaxAuthorizationEntries(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			input := "registries: [example.com]\nca_dir: ca\ncache: {dir: cache, max_authorization_entries: " + value + "}\n"
			if _, err := Load(writeConfig(t, input)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLoadPreservesDefaultForNullNumericLimit(t *testing.T) {
	c, err := Load(writeConfig(t, `registries: [example.com]
ca_dir: ca
cache: {dir: cache, max_fills: null}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxFills != DefaultCacheMaxFills {
		t.Fatalf("max_fills null should preserve default: %d", c.Cache.MaxFills)
	}
}

func TestLoadAcceptsLargeValidInteger(t *testing.T) {
	c, err := Load(writeConfig(t, `registries: [example.com]
ca_dir: ca
cache: {dir: cache, max_bytes: 34359738368}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxBytes != 34359738368 {
		t.Fatalf("max_bytes: %d", c.Cache.MaxBytes)
	}
}

func TestLoadAcceptsSignedInteger(t *testing.T) {
	c, err := Load(writeConfig(t, `registries: [example.com]
ca_dir: ca
cache: {dir: cache, max_fills: +7}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxFills != 7 {
		t.Fatalf("max_fills +7 should load as 7: %d", c.Cache.MaxFills)
	}
}
