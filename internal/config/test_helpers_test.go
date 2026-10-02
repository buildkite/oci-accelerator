package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func environmentLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[strings.TrimPrefix(name, "OCI_ACCELERATOR_")]
		return value, ok
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
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
