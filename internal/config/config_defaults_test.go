package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndRelativePaths(t *testing.T) {
	path := writeConfig(t, `registries: [EXAMPLE.com.:443, '[::1]:5000']
ca_dir: ca
cache:
  dir: cache
  max_bytes: null
dial_timeout: null
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	assertEqual(t, "Listen", c.Listen, "127.0.0.1:3128")
	assertEqual(t, "CADir", c.CADir, filepath.Join(base, "ca"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(base, "cache"))
	assertEqual(t, "Cache.MaxBytes", c.Cache.MaxBytes, int64(DefaultCacheMaxBytes))
	assertEqual(t, "Cache.MaxFills", c.Cache.MaxFills, DefaultCacheMaxFills)
	assertEqual(t, "Cache.MaxEntries", c.Cache.MaxEntries, DefaultCacheMaxEntries)
	assertEqual(t, "Cache.FillTimeout", c.Cache.FillTimeout, DefaultCacheFillTimeout)
	assertEqual(t, "Cache.FillWaitTimeout", c.Cache.FillWaitTimeout, time.Second)
	assertEqual(t, "Cache.AuthorizationTTL", c.Cache.AuthorizationTTL, 30*time.Second)
	assertEqual(t, "Cache.MaxAuthorizationEntries", c.Cache.MaxAuthorizationEntries, 10000)
	assertEqual(t, "DialTimeout", c.DialTimeout, 10*time.Second)
	assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 10*time.Second)
	assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 30*time.Second)
	assertEqual(t, "IdleTimeout", c.IdleTimeout, 2*time.Minute)
	assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 15*time.Second)
	assertEqual(t, "MaxConnections", c.MaxConnections, 256)
	assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 256)
	if c.Registries[0].String() != "example.com" || c.Registries[1].String() != "[::1]:5000" {
		t.Fatalf("registries: %v", c.Registries)
	}
	if len(c.RedirectAllowlist) != 0 {
		t.Fatalf("redirect allowlist should default empty: %v", c.RedirectAllowlist)
	}
}

func TestLoadExplicitOperationalOverrides(t *testing.T) {
	path := writeConfig(t, `listen: ':8080'
registries: [example.com]
ca_dir: /ca
cache:
  dir: /cache
  max_bytes: 12345
  max_fills: 7
  max_entries: 99
  fill_timeout: 3m
  fill_wait_timeout: 0s
  authorization_ttl: 0s
  max_authorization_entries: 42
dial_timeout: 1s
tls_handshake_timeout: 2s
response_header_timeout: 3s
idle_timeout: 4s
shutdown_timeout: 5s
max_connections: 6
max_leaf_certificates: 8
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "Listen", c.Listen, ":8080")
	assertEqual(t, "CADir", c.CADir, "/ca")
	assertEqual(t, "Cache.Dir", c.Cache.Dir, "/cache")
	assertEqual(t, "Cache.MaxBytes", c.Cache.MaxBytes, int64(12345))
	assertEqual(t, "Cache.MaxFills", c.Cache.MaxFills, 7)
	assertEqual(t, "Cache.MaxEntries", c.Cache.MaxEntries, 99)
	assertEqual(t, "Cache.FillTimeout", c.Cache.FillTimeout, 3*time.Minute)
	assertEqual(t, "Cache.FillWaitTimeout", c.Cache.FillWaitTimeout, time.Duration(0))
	assertEqual(t, "Cache.AuthorizationTTL", c.Cache.AuthorizationTTL, time.Duration(0))
	assertEqual(t, "Cache.MaxAuthorizationEntries", c.Cache.MaxAuthorizationEntries, 42)
	assertEqual(t, "DialTimeout", c.DialTimeout, time.Second)
	assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 2*time.Second)
	assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 3*time.Second)
	assertEqual(t, "IdleTimeout", c.IdleTimeout, 4*time.Second)
	assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 5*time.Second)
	assertEqual(t, "MaxConnections", c.MaxConnections, 6)
	assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 8)
}
