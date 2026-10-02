package config

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestLoadRejectsInvalidEnvironmentValuesSafely(t *testing.T) {
	tests := []struct{ field, value, context string }{
		{"REGISTRIES", "", "registries"}, {"REGISTRIES", "example.com,", "REGISTRIES"}, {"REGISTRIES", "example.com,EXAMPLE.COM.:443", "duplicate"},
		{"REDIRECT_ALLOWLIST", "user:secret-value@cdn.example", "redirect_allowlist"}, {"CA_DIR", "", "ca_dir"}, {"CACHE_DIR", " \t", "cache.dir"}, {"LISTEN", "secret-value", "listen"},
		{"METRICS_LISTEN", "0.0.0.0:9090", "loopback"}, {"METRICS_ALLOW_REMOTE", "TRUE", "METRICS_ALLOW_REMOTE"},
		{"CACHE_MAX_BYTES", "9223372036854775808", "CACHE_MAX_BYTES"}, {"CACHE_MAX_FILLS", "01", "CACHE_MAX_FILLS"}, {"CACHE_MAX_FILLS", "0x10", "CACHE_MAX_FILLS"}, {"CACHE_MAX_FILLS", "1_000", "CACHE_MAX_FILLS"}, {"CACHE_MAX_FILLS", "1.5", "CACHE_MAX_FILLS"},
		{"CACHE_MAX_FILLS", "+5", "without a sign"}, {"CACHE_MAX_FILLS", "-0", "without a sign"}, {"MAX_LEAF_CERTIFICATES", "-1", "without a sign"}, {"CACHE_MAX_FILLS", "00", "without a sign"}, {"CACHE_MAX_FILLS", "", "without a sign"},
		{"MAX_CONNECTIONS", "0", "max_connections"}, {"DIAL_TIMEOUT", "secret-value", "DIAL_TIMEOUT"}, {"CACHE_FILL_TIMEOUT", "0s", "cache.fill_timeout"}, {"CACHE_FILL_WAIT_TIMEOUT", "-1s", "cache.fill_wait_timeout"}, {"CACHE_AUTHORIZATION_TTL", "-1s", "cache.authorization_ttl"}, {"CACHE_ANONYMOUS_AUTHORIZATION_TTL", "-1s", "cache.anonymous_authorization_ttl"},
	}
	for _, tt := range tests {
		t.Run(tt.field+"="+tt.value, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache"}
			values[tt.field] = tt.value
			_, err := load("", environmentLookup(values))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.context) || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadAcceptsMaximumInt64(t *testing.T) {
	values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "CACHE_MAX_BYTES": "9223372036854775807"}
	c, err := load("", environmentLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "CacheMaxBytes", c.Cache.MaxBytes, int64(math.MaxInt64))
}

func TestAuthorizationTTLZerosAreIndependent(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		auth, anon  int64
	}{{"credentialed", "CACHE_AUTHORIZATION_TTL", 0, int64(DefaultCacheAnonymousAuthorizationTTL)}, {"anonymous", "CACHE_ANONYMOUS_AUTHORIZATION_TTL", int64(DefaultCacheAuthorizationTTL), 0}} {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", tt.field: "0s"}
			c, err := load("", environmentLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			if int64(c.Cache.AuthorizationTTL) != tt.auth || int64(c.Cache.AnonymousAuthorizationTTL) != tt.anon {
				t.Fatalf("TTLs = %v, %v", c.Cache.AuthorizationTTL, c.Cache.AnonymousAuthorizationTTL)
			}
		})
	}
}
