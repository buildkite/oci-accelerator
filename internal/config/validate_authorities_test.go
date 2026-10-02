package config

import (
	"strings"
	"testing"
)

func TestAuthorityCanonicalization(t *testing.T) {
	c, err := load("", environmentLookup(map[string]string{"REGISTRIES": "REGISTRY.EXAMPLE.:443,[2001:0DB8::1]:5000", "REDIRECT_ALLOWLIST": "CDN.EXAMPLE.:443,cdn.example:8443,[::1]:5000", "CA_DIR": "ca", "CACHE_DIR": "cache"}))
	if err != nil {
		t.Fatal(err)
	}
	wantRegistries := []string{"registry.example", "[2001:db8::1]:5000"}
	wantRedirects := []string{"cdn.example", "cdn.example:8443", "[::1]:5000"}
	for i, want := range wantRegistries {
		assertEqual(t, "registry", c.Registries[i].String(), want)
	}
	for i, want := range wantRedirects {
		assertEqual(t, "redirect", c.RedirectAllowlist[i].String(), want)
	}
}

func TestAuthoritiesRejectInvalidEntries(t *testing.T) {
	for _, tt := range []struct{ field, value string }{{"REGISTRIES", ""}, {"REGISTRIES", "example.com,"}, {"REGISTRIES", "example.com, ,other.example"}, {"REGISTRIES", "example.com,EXAMPLE.COM.:443"}, {"REGISTRIES", "user:secret@example.com"}, {"REDIRECT_ALLOWLIST", "cdn.example,"}, {"REDIRECT_ALLOWLIST", "user:secret@cdn.example"}, {"REDIRECT_ALLOWLIST", "cdn.example:0"}, {"REDIRECT_ALLOWLIST", "cdn.example:99999"}, {"REDIRECT_ALLOWLIST", "cdn.example,CDN.EXAMPLE.:443"}, {"REDIRECT_ALLOWLIST", "cdn.example:8443,CDN.EXAMPLE.:08443"}} {
		t.Run(tt.field+tt.value, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache"}
			values[tt.field] = tt.value
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, "", tt.field)
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "cdn.example") {
				t.Fatalf("error echoed value: %v", err)
			}
		})
	}
}
