package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Clear only application settings before tests inject their own overrides.
	for _, setting := range environmentSettings(&rawConfig{}) {
		if err := os.Unsetenv(environmentPrefix + setting.name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

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

// assertInvalidSetting requires an ErrInvalid error that starts with the full
// variable names for settings and contains reason.
func assertInvalidSetting(t *testing.T, err error, reason string, settings ...string) {
	t.Helper()
	names := make([]string, len(settings))
	for i, setting := range settings {
		names[i] = environmentPrefix + setting
	}
	prefix := "invalid configuration: " + strings.Join(names, " and ") + ": "
	if !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("error = %v, want prefix %q and reason %q", err, prefix, reason)
	}
}

func assertEqual[T comparable](t *testing.T, field string, actual, expected T) {
	t.Helper()
	if actual != expected {
		t.Errorf("%s = %v, want %v", field, actual, expected)
	}
}
