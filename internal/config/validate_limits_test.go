package config

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateLimitsRequirePositiveValues(t *testing.T) {
	for _, field := range []string{"CACHE_MAX_BYTES", "CACHE_MAX_FILLS", "CACHE_MAX_ENTRIES", "CACHE_MAX_AUTHORIZATION_ENTRIES", "MAX_CONNECTIONS", "MAX_LEAF_CERTIFICATES"} {
		t.Run(field, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", field: "0"}
			_, err := load("", environmentLookup(values))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "must be positive") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDurationBounds(t *testing.T) {
	for _, field := range []string{"DIAL_TIMEOUT", "TLS_HANDSHAKE_TIMEOUT", "RESPONSE_HEADER_TIMEOUT", "IDLE_TIMEOUT", "SHUTDOWN_TIMEOUT", "CACHE_FILL_TIMEOUT"} {
		for _, value := range []string{"0s", "-1ns"} {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", field: value}
			if _, err := load("", environmentLookup(values)); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s=%s: %v", field, value, err)
			}
		}
	}
	for _, field := range []string{"CACHE_FILL_WAIT_TIMEOUT", "CACHE_AUTHORIZATION_TTL", "CACHE_ANONYMOUS_AUTHORIZATION_TTL"} {
		values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache", field: "-1ns"}
		if _, err := load("", environmentLookup(values)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", field, err)
		}
	}
}
