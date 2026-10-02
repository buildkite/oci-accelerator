package config

import (
	"errors"
	"strings"
	"testing"
)

func TestRequiredPaths(t *testing.T) {
	for _, tt := range []struct{ field, value, context string }{{"CA_DIR", "", "ca_dir"}, {"CA_DIR", " \t", "ca_dir"}, {"CA_DIR", "a\x00b", "ca_dir"}, {"CACHE_DIR", "", "cache.dir"}, {"CACHE_DIR", "\u2003", "cache.dir"}, {"CACHE_DIR", "a\x00b", "cache.dir"}} {
		t.Run(tt.field, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache"}
			values[tt.field] = tt.value
			_, err := load("", environmentLookup(values))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.context) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStateDirectoryRelationships(t *testing.T) {
	for _, tt := range []struct {
		name, ca, cache string
		valid           bool
	}{{"identical", "state", "./state", false}, {"CA nested", "state/ca", "state", false}, {"normalized nested", "state/other/../ca", "state", false}, {"siblings", "ca", "cache", true}, {"cache inside CA", "ca", "ca/cache", true}, {"prefix sibling", "cache-other", "cache", true}} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": tt.ca, "CACHE_DIR": tt.cache}))
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
