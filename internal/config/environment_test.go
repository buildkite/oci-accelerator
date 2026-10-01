package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadEnvironmentSettings(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	lookup := environmentLookup(map[string]string{
		"LISTEN": "[::1]:4321", "METRICS_LISTEN": "0.0.0.0:9091", "METRICS_ALLOW_REMOTE": "true",
		"REGISTRIES": " GHCR.IO.:443, [2001:db8::1]:5000 ", "REDIRECT_ALLOWLIST": "cdn.example:8443",
		"CA_DIR": "env-ca", "CACHE_DIR": "env-cache",
		"CACHE_MAX_BYTES": "4294967297", "CACHE_MAX_FILLS": "3", "CACHE_MAX_ENTRIES": "47",
		"CACHE_FILL_TIMEOUT": "7m", "CACHE_FILL_WAIT_TIMEOUT": "250ms",
		"CACHE_AUTHORIZATION_TTL": "9s", "CACHE_ANONYMOUS_AUTHORIZATION_TTL": "13m",
		"CACHE_MAX_AUTHORIZATION_ENTRIES": "59", "DIAL_TIMEOUT": "2s",
		"TLS_HANDSHAKE_TIMEOUT": "3s", "RESPONSE_HEADER_TIMEOUT": "4s", "IDLE_TIMEOUT": "5s",
		"SHUTDOWN_TIMEOUT": "6s", "MAX_CONNECTIONS": "17", "MAX_LEAF_CERTIFICATES": "19",
	})
	// An incomplete file can get required fields from the environment. A list
	// override must replace the file's list, not retain any of its authorities.
	path := writeConfig(t, "registries: [file.example]\nredirect_allowlist: [file-cdn.example]\nlisten: ':1234'\ncache: {max_fills: 2}\n")
	for _, path := range []string{"", path} {
		c, err := load(path, lookup)
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, "Listen", c.Listen, "[::1]:4321")
		assertEqual(t, "MetricsListen", c.MetricsListen, "0.0.0.0:9091")
		assertEqual(t, "CADir", c.CADir, filepath.Join(root, "env-ca"))
		assertEqual(t, "Cache", c.Cache, Cache{
			Dir: filepath.Join(root, "env-cache"), MaxBytes: 4294967297, MaxFills: 3, MaxEntries: 47,
			FillTimeout: 7 * time.Minute, FillWaitTimeout: 250 * time.Millisecond,
			AuthorizationTTL: 9 * time.Second, AnonymousAuthorizationTTL: 13 * time.Minute,
			MaxAuthorizationEntries: 59,
		})
		assertEqual(t, "DialTimeout", c.DialTimeout, 2*time.Second)
		assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 3*time.Second)
		assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 4*time.Second)
		assertEqual(t, "IdleTimeout", c.IdleTimeout, 5*time.Second)
		assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 6*time.Second)
		assertEqual(t, "MaxConnections", c.MaxConnections, 17)
		assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 19)
		if len(c.Registries) != 2 || c.Registries[0].String() != "ghcr.io" || c.Registries[1].String() != "[2001:db8::1]:5000" {
			t.Fatalf("registry replacement = %v", c.Registries)
		}
		if len(c.RedirectAllowlist) != 1 || c.RedirectAllowlist[0].String() != "cdn.example:8443" {
			t.Fatalf("redirect replacement = %v", c.RedirectAllowlist)
		}
	}
}

func TestLoadEnvironmentEmptyAndZeroOverrides(t *testing.T) {
	path := writeConfig(t, validConfig+`metrics_listen: 0.0.0.0:9090
metrics_allow_remote: true
redirect_allowlist: [cdn.example]
dial_timeout: 7s
`)
	c, err := load(path, environmentLookup(map[string]string{
		"METRICS_LISTEN": "", "METRICS_ALLOW_REMOTE": "false", "REDIRECT_ALLOWLIST": "",
		"CACHE_FILL_WAIT_TIMEOUT": "0s", "CACHE_AUTHORIZATION_TTL": "0s", "CACHE_ANONYMOUS_AUTHORIZATION_TTL": "0s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsListen != "" || len(c.RedirectAllowlist) != 0 || c.Cache.FillWaitTimeout != 0 || c.Cache.AuthorizationTTL != 0 || c.Cache.AnonymousAuthorizationTTL != 0 {
		t.Fatalf("empty or zero override was ignored: %+v", c)
	}
	assertEqual(t, "unset retains YAML", c.DialTimeout, 7*time.Second)
	assertEqual(t, "unset retains default", c.MaxConnections, 256)
	// Explicit false must remove the file's remote-metrics permission.
	_, err = load(path, environmentLookup(map[string]string{"METRICS_ALLOW_REMOTE": "false"}))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote metrics accepted without opt-in: %v", err)
	}
}

func TestLoadEnvironmentPathBases(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := writeConfig(t, validConfig)
	for _, field := range []string{"CA_DIR", "CACHE_DIR"} {
		c, err := load(path, environmentLookup(map[string]string{field: "env-state"}))
		if err != nil {
			t.Fatal(err)
		}
		wantCA, wantCache := filepath.Join(filepath.Dir(path), "ca"), filepath.Join(filepath.Dir(path), "cache")
		if field == "CA_DIR" {
			wantCA = filepath.Join(root, "env-state")
		} else {
			wantCache = filepath.Join(root, "env-state")
		}
		assertEqual(t, "CADir", c.CADir, wantCA)
		assertEqual(t, "Cache.Dir", c.Cache.Dir, wantCache)
	}
	// Lexically identical paths from different sources must still conflict.
	_, err := load(path, environmentLookup(map[string]string{"CA_DIR": filepath.Join(filepath.Dir(path), "cache", "child")}))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "must not be inside") {
		t.Fatalf("nested mixed-source state paths accepted: %v", err)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	path := writeConfig(t, validConfig)
	for _, test := range []struct {
		field, value, context string
	}{
		{"REGISTRIES", "", "registries"},
		{"REGISTRIES", "example.com,", "REGISTRIES"},
		{"REGISTRIES", "example.com, ,other.example", "REGISTRIES"},
		{"REGISTRIES", "example.com,EXAMPLE.COM.:443", "duplicate authority"},
		{"REDIRECT_ALLOWLIST", " ", "REDIRECT_ALLOWLIST"},
		{"REDIRECT_ALLOWLIST", "user:secret-value@cdn.example", "redirect_allowlist"},
		{"CA_DIR", "", "ca_dir"},
		{"CACHE_DIR", " \t", "cache.dir"},
		{"LISTEN", "", "listen"},
		{"LISTEN", "secret-value", "listen"},
		{"METRICS_LISTEN", "0.0.0.0:9090", "loopback"},
		{"METRICS_ALLOW_REMOTE", "1", "METRICS_ALLOW_REMOTE"},
		{"METRICS_ALLOW_REMOTE", "TRUE", "METRICS_ALLOW_REMOTE"},
		{"METRICS_ALLOW_REMOTE", "", "METRICS_ALLOW_REMOTE"},
		{"CACHE_MAX_BYTES", "9223372036854775808", "CACHE_MAX_BYTES"},
		{"CACHE_MAX_FILLS", "01", "CACHE_MAX_FILLS"},
		{"CACHE_MAX_FILLS", "0x10", "CACHE_MAX_FILLS"},
		{"CACHE_MAX_FILLS", "1_000", "CACHE_MAX_FILLS"},
		{"CACHE_MAX_FILLS", "1.5", "CACHE_MAX_FILLS"},
		{"MAX_CONNECTIONS", "", "MAX_CONNECTIONS"},
		{"MAX_CONNECTIONS", "0", "max_connections"},
		{"MAX_LEAF_CERTIFICATES", "-1", "max_leaf_certificates"},
		{"DIAL_TIMEOUT", "secret-value", "DIAL_TIMEOUT"},
		{"DIAL_TIMEOUT", "", "DIAL_TIMEOUT"},
		{"CACHE_FILL_TIMEOUT", "0s", "cache.fill_timeout"},
		{"CACHE_FILL_WAIT_TIMEOUT", "-1s", "cache.fill_wait_timeout"},
		{"CACHE_AUTHORIZATION_TTL", "-1s", "cache.authorization_ttl"},
		{"CACHE_ANONYMOUS_AUTHORIZATION_TTL", "-1s", "cache.anonymous_authorization_ttl"},
	} {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			_, err := load(path, environmentLookup(map[string]string{test.field: test.value}))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), test.context) || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected safe field-specific error, got %v", err)
			}
		})
	}
}

func TestLoadEnvironmentCannotHideInvalidYAML(t *testing.T) {
	lookup := environmentLookup(map[string]string{
		"REGISTRIES": "example.com", "CA_DIR": "/ca", "CACHE_DIR": "/cache", "MAX_CONNECTIONS": "4",
	})
	for _, input := range []string{
		"", "[", "unknown: true", "max_connections: secret-value", "max_connections: 1\nmax_connections: 2",
		"{}\n---\n{}",
	} {
		_, err := load(writeConfig(t, input), lookup)
		if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("YAML %q: expected safe error, got %v", input, err)
		}
	}
	if _, err := load(filepath.Join(t.TempDir(), "missing.yaml"), lookup); err == nil {
		t.Fatal("environment hid missing configuration file")
	}
}

func environmentLookup(values map[string]string) func(string) (string, bool) {
	env := make(map[string]string, len(values))
	for name, value := range values {
		env["OCI_ACCELERATOR_"+name] = value
	}
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}
