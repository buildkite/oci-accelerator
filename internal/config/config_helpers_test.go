package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validConfig = `registries: [example.com]
ca_dir: ca
cache: {dir: cache}
`

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
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
