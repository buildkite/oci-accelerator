package config

import "testing"

func TestRequiredPaths(t *testing.T) {
	for _, tt := range []struct{ field, value, reason string }{{"CA_DIR", "", "non-blank"}, {"CA_DIR", " \t", "non-blank"}, {"CA_DIR", "a\x00b", "NUL"}, {"CACHE_DIR", "", "non-blank"}, {"CACHE_DIR", "\u2003", "non-blank"}, {"CACHE_DIR", "a\x00b", "NUL"}} {
		t.Run(tt.field, func(t *testing.T) {
			values := map[string]string{"REGISTRIES": "example.com", "CA_DIR": "ca", "CACHE_DIR": "cache"}
			values[tt.field] = tt.value
			_, err := load("", environmentLookup(values))
			assertInvalidSetting(t, err, tt.reason, tt.field)
		})
	}
}

func TestStateDirectoryRelationships(t *testing.T) {
	for _, tt := range []struct {
		name, ca, cache, reason string
	}{{"identical", "state", "./state", "must be distinct"}, {"CA nested", "state/ca", "state", "must not be inside"}, {"normalized nested", "state/other/../ca", "state", "must not be inside"}, {"siblings", "ca", "cache", ""}, {"cache inside CA", "ca", "ca/cache", ""}, {"prefix sibling", "cache-other", "cache", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load("", environmentLookup(map[string]string{"REGISTRIES": "example.com", "CA_DIR": tt.ca, "CACHE_DIR": tt.cache}))
			if tt.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			assertInvalidSetting(t, err, tt.reason, "CA_DIR", "CACHE_DIR")
		})
	}
}
