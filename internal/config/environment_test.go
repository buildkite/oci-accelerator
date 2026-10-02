package config

import (
	"math"
	"strings"
	"testing"
)

func TestLoadRejectsInvalidEnvironmentValuesSafely(t *testing.T) {
	tests := []struct{ field, value, reason string }{
		{"REGISTRIES", "", "at least one authority"}, {"REGISTRIES", "example.com,", "empty list entries"}, {"REGISTRIES", "example.com,EXAMPLE.COM.:443", "element 2: duplicate"},
		{"REDIRECT_ALLOWLIST", "user:secret-value@cdn.example", "element 1: must be a hostname"}, {"CA_DIR", "", "non-blank"}, {"CACHE_DIR", " \t", "non-blank"}, {"LISTEN", "secret-value", "host:port"},
		{"METRICS_LISTEN", "0.0.0.0:9090", "loopback"}, {"METRICS_ALLOW_REMOTE", "TRUE", "true or false"},
		{"CACHE_MAX_BYTES", "9223372036854775808", "int64 range"}, {"CACHE_MAX_FILLS", "01", "leading zeros"}, {"CACHE_MAX_FILLS", "0x10", "decimal integer"}, {"CACHE_MAX_FILLS", "1_000", "decimal integer"}, {"CACHE_MAX_FILLS", "1.5", "decimal integer"},
		{"CACHE_MAX_FILLS", "+5", "without a sign"}, {"CACHE_MAX_FILLS", "-0", "without a sign"}, {"MAX_LEAF_CERTIFICATES", "-1", "without a sign"}, {"CACHE_MAX_FILLS", "00", "without a sign"}, {"CACHE_MAX_FILLS", "", "without a sign"},
		{"MAX_CONNECTIONS", "0", "must be positive"}, {"DIAL_TIMEOUT", "secret-value", "duration string"}, {"CACHE_FILL_TIMEOUT", "0s", "must be positive"}, {"CACHE_FILL_WAIT_TIMEOUT", "-1s", "non-negative"}, {"CACHE_AUTHORIZATION_TTL", "-1s", "non-negative"}, {"CACHE_ANONYMOUS_AUTHORIZATION_TTL", "-1s", "non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.field+"="+tt.value, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache"}
			values[tt.field] = tt.value
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, tt.reason, tt.field)
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error echoed value: %v", err)
			}
		})
	}
}

// Parse and validation failures for the same setting must name it identically.
func TestLoadNamesSettingConsistentlyAcrossParseAndValidation(t *testing.T) {
	for _, tt := range []struct{ stage, value, reason string }{{"parse", "abc", "decimal integer"}, {"validation", "0", "must be positive"}} {
		t.Run(tt.stage, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "MAX_CONNECTIONS": tt.value}
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, tt.reason, "MAX_CONNECTIONS")
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
