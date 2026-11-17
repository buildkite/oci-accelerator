package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPrecedenceAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := writeConfig(t, "OCI_ACCELERATOR_REGISTRIES=file.example\r\nOCI_ACCELERATOR_CA_DIR='ca#literal' # note\r\nOCI_ACCELERATOR_CACHE_DIR=cache\r\nOCI_ACCELERATOR_REDIRECT_ALLOWLIST=from-file\r\n")
	t.Setenv("OCI_ACCELERATOR_REGISTRIES", "process.example")
	t.Setenv("OCI_ACCELERATOR_REDIRECT_ALLOWLIST", "")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if c.Registries[0].String() != "process.example" || len(c.RedirectAllowlist) != 0 {
		t.Fatalf("process overrides not applied: %+v", c)
	}
	assertEqual(t, "CADir", c.CADir, filepath.Join(root, "ca#literal"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(root, "cache"))
}

func TestLoadParsesOnlyEffectiveValuesAfterFileSyntax(t *testing.T) {
	path := writeConfig(t, "OCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\nOCI_ACCELERATOR_MAX_CONNECTIONS=2\n")
	t.Setenv("OCI_ACCELERATOR_MAX_CONNECTIONS", "3")
	_, err := Load(path)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("override hid file error: %v", err)
	}

	path = writeConfig(t, validEnv+"OCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("effective process value did not override invalid file value: %v", err)
	}
	assertEqual(t, "MaxConnections", c.MaxConnections, 3)
}

func TestLoadFileErrorClassification(t *testing.T) {
	_, err := Load(writeConfig(t, "SECRET_NAME=secret-value\n"))
	if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "SECRET_NAME") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("invalid file error = %v", err)
	}

	_, err = Load(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrInvalid) {
		t.Fatalf("file I/O error = %v", err)
	}
}

func TestLoadDoesNotMutateEnvironmentOrDiscoverFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".env", []byte(validEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	before := os.Environ()
	if _, err := Load(".env"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(before, "\x00") != strings.Join(os.Environ(), "\x00") {
		t.Fatal("Load mutated process environment")
	}

	_, err := Load("")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "OCI_ACCELERATOR_REGISTRIES") {
		t.Fatalf("dotenv was discovered: %v", err)
	}
}
