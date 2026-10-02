package config

import (
	"errors"
	"testing"
)

func TestLoadCADirPrecedence(t *testing.T) {
	path := writeConfig(t, "OCI_ACCELERATOR_CA_DIR=file-ca\n")
	tests := []struct {
		name, path string
		values     map[string]string
		want       string
	}{
		{"unset", "", nil, ""},
		{"environment without file", "", map[string]string{"CA_DIR": "env-ca"}, "env-ca"},
		{"file", path, nil, "file-ca"},
		{"environment", path, map[string]string{"CA_DIR": "env-ca"}, "env-ca"},
		{"empty environment", path, map[string]string{"CA_DIR": ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := loadCADir(tt.path, environmentLookup(tt.values))
			if err != nil {
				t.Fatal(err)
			}
			assertEqual(t, "CA directory", dir, tt.want)
		})
	}
}

func TestLoadCADirIgnoresOtherSettings(t *testing.T) {
	// Each other setting would fail Load, including a cache directory equal to the CA directory.
	path := writeConfig(t, "OCI_ACCELERATOR_CA_DIR=ca\nOCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\n")
	dir, err := loadCADir(path, environmentLookup(map[string]string{"CACHE_DIR": "ca", "LISTEN": "invalid"}))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "CA directory", dir, "ca")
}

func TestLoadCADirRejectsInvalidEnvFile(t *testing.T) {
	path := writeConfig(t, "OCI_ACCELERATOR_UNKNOWN=value\n")
	if _, err := loadCADir(path, environmentLookup(map[string]string{"CA_DIR": "ca"})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}
