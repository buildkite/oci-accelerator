package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsAndRelativePaths(t *testing.T) {
	path := writeConfig(t, "registries: [EXAMPLE.com.:443, '[::1]:5000']\nca_dir: ca\ncache:\n  dir: cache\n  max_bytes: null\ndial_timeout: null\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	if c.Listen != "127.0.0.1:3128" || c.CADir != filepath.Join(base, "ca") || c.Cache.Dir != filepath.Join(base, "cache") || c.Cache.MaxBytes != DefaultCacheMaxBytes {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DialTimeout != 10*time.Second || c.TLSHandshakeTimeout != 10*time.Second || c.ResponseHeaderTimeout != 30*time.Second || c.IdleTimeout != 2*time.Minute || c.ShutdownTimeout != 15*time.Second || c.MaxConnections != 256 || c.MaxLeafCertificates != 256 {
		t.Fatalf("unexpected operational defaults: %+v", c)
	}
	if c.Registries[0].String() != "example.com" || c.Registries[1].String() != "[::1]:5000" {
		t.Fatalf("registries: %v", c.Registries)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := []string{
		"ca_dir: ca\n",                // registries required
		"registries: [example.com]\n", // CA required
		"registries: [example.com]\nca_dir: ca\nunknown: true\n",
		"registries: [example.com]\nregistries: [other.example]\nca_dir: ca\n",
		"registries: [example.com, EXAMPLE.COM:443]\nca_dir: ca\n",
		"registries: [user:pass@example.com]\nca_dir: ca\n",
		"registries: [example.com]\nca_dir: ca\ndial_timeout: 0s\n",
		"registries: [example.com]\nca_dir: ca\nmax_connections: 0\n",
		"registries: [example.com]\nca_dir: ca\nlisten: '127.0.0.1:99999'\n",
		"registries: [example.com]\nca_dir: ca\nlisten: '127.0.0.1:'\n",
		"registries: [example.com]\nca_dir: ca\nlisten: 'bad host:3128'\n",
		"registries: [example.com]\nca_dir: ca\n---\n{}\n",
		"registries: [example.com\nca_dir: secret-value\n",
	}
	for _, input := range tests {
		_, err := Load(writeConfig(t, input))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Load unexpectedly returned %v", err)
		}
		if err != nil && (strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "user:pass")) {
			t.Errorf("error leaked input: %v", err)
		}
	}
}
