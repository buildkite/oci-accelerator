package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsInvalidListenerSafely(t *testing.T) {
	values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", "LISTEN": "secret-value"}
	_, err := load("", environmentLookup(values))
	assertInvalidSetting(t, err, "host:port", "LISTEN")
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error echoed value: %v", err)
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
