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
	if c.Listen != "127.0.0.1:3128" || c.CADir != filepath.Join(base, "ca") || c.Cache.Dir != filepath.Join(base, "cache") || c.Cache.MaxBytes != DefaultCacheMaxBytes || c.Cache.MaxFills != DefaultCacheMaxFills || c.Cache.MaxEntries != DefaultCacheMaxEntries || c.Cache.FillTimeout != DefaultCacheFillTimeout {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DialTimeout != 10*time.Second || c.TLSHandshakeTimeout != 10*time.Second || c.ResponseHeaderTimeout != 30*time.Second || c.IdleTimeout != 2*time.Minute || c.ShutdownTimeout != 15*time.Second || c.MaxConnections != 256 || c.MaxLeafCertificates != 256 {
		t.Fatalf("unexpected operational defaults: %+v", c)
	}
	if c.Registries[0].String() != "example.com" || c.Registries[1].String() != "[::1]:5000" {
		t.Fatalf("registries: %v", c.Registries)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist should default empty: %v", c.RedirectAllowlist)
	}
}

func TestLoadExplicitOperationalOverrides(t *testing.T) {
	path := writeConfig(t, "listen: ':8080'\nregistries: [example.com]\nca_dir: /ca\ncache: {dir: /cache, max_bytes: 12345, max_fills: 7, max_entries: 99, fill_timeout: 3m}\ndial_timeout: 1s\ntls_handshake_timeout: 2s\nresponse_header_timeout: 3s\nidle_timeout: 4s\nshutdown_timeout: 5s\nmax_connections: 6\nmax_leaf_certificates: 8\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.CADir != "/ca" || c.Cache.Dir != "/cache" || c.Cache.MaxBytes != 12345 || c.Cache.MaxFills != 7 || c.Cache.MaxEntries != 99 || c.Cache.FillTimeout != 3*time.Minute {
		t.Fatalf("cache/path overrides: %+v", c)
	}
	if c.DialTimeout != time.Second || c.TLSHandshakeTimeout != 2*time.Second || c.ResponseHeaderTimeout != 3*time.Second || c.IdleTimeout != 4*time.Second || c.ShutdownTimeout != 5*time.Second || c.MaxConnections != 6 || c.MaxLeafCertificates != 8 {
		t.Fatalf("operational overrides: %+v", c)
	}
}

func TestLoadRedirectAllowlist(t *testing.T) {
	path := writeConfig(t, "registries: [registry.example]\nredirect_allowlist: [CDN.EXAMPLE.:443, cdn.example:8443, '[2001:0DB8::1]', '[::1]:5000']\nca_dir: ca\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cdn.example", "cdn.example:8443", "[2001:db8::1]", "[::1]:5000"}
	if len(c.RedirectAllowlist) != len(want) {
		t.Fatalf("redirect allowlist: %v", c.RedirectAllowlist)
	}
	for i, authority := range c.RedirectAllowlist {
		if authority.String() != want[i] {
			t.Errorf("redirect allowlist[%d] = %q, want %q", i, authority.String(), want[i])
		}
	}
	if len(c.Registries) != 1 || c.Registries[0].String() != "registry.example" {
		t.Fatalf("redirect allowlist changed registries: %v", c.Registries)
	}
}

func TestLoadAllowsEmptyRedirectAllowlist(t *testing.T) {
	c, err := Load(writeConfig(t, "registries: [registry.example]\nredirect_allowlist: []\nca_dir: ca\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist: %v", c.RedirectAllowlist)
	}
}

func TestLoadRejectsInvalidRedirectAllowlist(t *testing.T) {
	tests := []string{
		"redirect_allowlist: [user:secret@cdn.example]",
		"redirect_allowlist: [cdn.example:0]",
		"redirect_allowlist: [cdn.example:99999]",
		"redirect_allowlist: [' cdn.example']",
		"redirect_allowlist: [cdn.example, CDN.EXAMPLE.:443]",
		"redirect_allowlist: [cdn.example:8443, CDN.EXAMPLE.:08443]",
	}
	for _, entry := range tests {
		input := "registries: [registry.example]\nca_dir: ca\n" + entry + "\n"
		_, err := Load(writeConfig(t, input))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Load(%q) unexpectedly returned %v", entry, err)
		}
		if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "cdn.example")) {
			t.Errorf("error leaked redirect allowlist input: %v", err)
		}
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
		"registries: [example.com]\nca_dir: ca\ncache: {max_fills: 0}\n",
		"registries: [example.com]\nca_dir: ca\ncache: {max_entries: -1}\n",
		"registries: [example.com]\nca_dir: ca\ncache: {fill_timeout: 0s}\n",
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
