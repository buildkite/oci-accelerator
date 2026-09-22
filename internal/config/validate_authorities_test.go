package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadRedirectAllowlist(t *testing.T) {
	path := writeConfig(t, `registries: [registry.example]
redirect_allowlist: [CDN.EXAMPLE.:443, cdn.example:8443, '[2001:0DB8::1]', '[::1]:5000']
ca_dir: ca
cache: {dir: cache}
`)
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
	c, err := Load(writeConfig(t, validConfig+"redirect_allowlist: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist: %v", c.RedirectAllowlist)
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
			input := validConfig + test.entry + "\n"
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

// TestLoadRejectsNullAndEmptyListElements covers registries and
// redirect_allowlist elements that are null (an error, per parseAuthorities,
// rather than a silently dropped entry) or blank, at both the sole and
// trailing positions of the list.
func TestLoadRejectsNullAndEmptyListElements(t *testing.T) {
	tests := []struct{ name, entry string }{
		{"registry_trailing_null", "registries: [example.com, null]"},
		{"registry_null", "registries: [null]"},
		{"registry_empty", "registries: ['']"},
		{"redirect_trailing_null", "registries: [example.com]\nredirect_allowlist: [cdn.example, null]"},
		{"redirect_null", "registries: [example.com]\nredirect_allowlist: [null]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "ca_dir: ca\ncache: {dir: cache}\n" + test.entry + "\n"
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
