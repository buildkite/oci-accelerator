package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const validEnv = "OCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=ca\nOCI_ACCELERATOR_CACHE_DIR=cache\n"

func TestLoadDefaults(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	c, err := load("", environmentLookup(map[string]string{"REGISTRIES": "EXAMPLE.com.:443,[::1]:5000", "CA_DIR": "ca", "CACHE_DIR": "cache"}))
	if err != nil {
		t.Fatal(err)
	}

	wantCache := Cache{Dir: filepath.Join(root, "cache"), MaxBytes: DefaultCacheMaxBytes, MaxFills: DefaultCacheMaxFills, MaxEntries: DefaultCacheMaxEntries, FillTimeout: DefaultCacheFillTimeout, FillWaitTimeout: time.Second, MutableTTL: 30 * time.Second, MutableShared: true, AuthorizationTTL: 30 * time.Second, AnonymousAuthorizationTTL: 15 * time.Minute, MaxAuthorizationEntries: 10000}
	assertEqual(t, "Listen", c.Listen, "127.0.0.1:3128")
	assertEqual(t, "CADir", c.CADir, filepath.Join(root, "ca"))
	assertEqual(t, "Cache", c.Cache, wantCache)
	assertEqual(t, "Peer.HintRefreshTimeout", c.Peer.HintRefreshTimeout, time.Minute)
	assertEqual(t, "Peer.HintRefreshInterval", c.Peer.HintRefreshInterval, time.Minute)
	assertEqual(t, "Peer.HintFullRefreshInterval", c.Peer.HintFullRefreshInterval, time.Hour)
	assertEqual(t, "DialTimeout", c.DialTimeout, 10*time.Second)
	assertEqual(t, "TLSHandshakeTimeout", c.TLSHandshakeTimeout, 10*time.Second)
	assertEqual(t, "ResponseHeaderTimeout", c.ResponseHeaderTimeout, 30*time.Second)
	assertEqual(t, "IdleTimeout", c.IdleTimeout, 2*time.Minute)
	assertEqual(t, "ShutdownTimeout", c.ShutdownTimeout, 15*time.Second)
	assertEqual(t, "MaxConnections", c.MaxConnections, 256)
	assertEqual(t, "MaxLeafCertificates", c.MaxLeafCertificates, 256)
	if c.Registries[0].String() != "example.com" || c.Registries[1].String() != "[::1]:5000" || len(c.RedirectAllowlist) != 0 {
		t.Fatalf("authorities = %v, %v", c.Registries, c.RedirectAllowlist)
	}
}

func TestLoadAllSettingOverrides(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	c, err := load("", environmentLookup(map[string]string{
		"LISTEN": "[::1]:4321", "METRICS_LISTEN": "0.0.0.0:9091", "METRICS_ALLOW_REMOTE": "true",
		"REGISTRIES": "ghcr.io", "REDIRECT_ALLOWLIST": "cdn.example:8443", "CA_DIR": "ca2", "CACHE_DIR": "cache2",
		"CACHE_MAX_BYTES": "4294967297", "CACHE_MAX_FILLS": "3", "CACHE_MAX_ENTRIES": "47", "CACHE_FILL_TIMEOUT": "7m",
		"CACHE_FILL_WAIT_TIMEOUT": "250ms", "CACHE_MUTABLE_TTL": "17s", "CACHE_MUTABLE_SHARED": "false", "CACHE_AUTHORIZATION_TTL": "9s", "CACHE_ANONYMOUS_AUTHORIZATION_TTL": "13m", "CACHE_MAX_AUTHORIZATION_ENTRIES": "59",
		"PEER_HINT_REFRESH_TIMEOUT": "17s", "PEER_HINT_REFRESH_INTERVAL": "23s", "PEER_HINT_FULL_REFRESH_INTERVAL": "2h",
		"DIAL_TIMEOUT": "2s", "TLS_HANDSHAKE_TIMEOUT": "3s", "RESPONSE_HEADER_TIMEOUT": "4s", "IDLE_TIMEOUT": "5s", "SHUTDOWN_TIMEOUT": "6s", "MAX_CONNECTIONS": "17", "MAX_LEAF_CERTIFICATES": "19",
	}))
	if err != nil {
		t.Fatal(err)
	}

	assertEqual(t, "Listen", c.Listen, "[::1]:4321")
	assertEqual(t, "MetricsListen", c.MetricsListen, "0.0.0.0:9091")
	assertEqual(t, "CADir", c.CADir, filepath.Join(root, "ca2"))
	assertEqual(t, "Cache", c.Cache, Cache{Dir: filepath.Join(root, "cache2"), MaxBytes: 4294967297, MaxFills: 3, MaxEntries: 47, FillTimeout: 7 * time.Minute, FillWaitTimeout: 250 * time.Millisecond, MutableTTL: 17 * time.Second, AuthorizationTTL: 9 * time.Second, AnonymousAuthorizationTTL: 13 * time.Minute, MaxAuthorizationEntries: 59})
	assertEqual(t, "Peer.HintRefreshTimeout", c.Peer.HintRefreshTimeout, 17*time.Second)
	assertEqual(t, "Peer.HintRefreshInterval", c.Peer.HintRefreshInterval, 23*time.Second)
	assertEqual(t, "Peer.HintFullRefreshInterval", c.Peer.HintFullRefreshInterval, 2*time.Hour)
	assertEqual(t, "DialTimeout", c.DialTimeout, 2*time.Second)
	assertEqual(t, "TLS", c.TLSHandshakeTimeout, 3*time.Second)
	assertEqual(t, "Response", c.ResponseHeaderTimeout, 4*time.Second)
	assertEqual(t, "Idle", c.IdleTimeout, 5*time.Second)
	assertEqual(t, "Shutdown", c.ShutdownTimeout, 6*time.Second)
	assertEqual(t, "Connections", c.MaxConnections, 17)
	assertEqual(t, "Certificates", c.MaxLeafCertificates, 19)
	if c.Registries[0].String() != "ghcr.io" || c.RedirectAllowlist[0].String() != "cdn.example:8443" {
		t.Fatalf("authorities = %v, %v", c.Registries, c.RedirectAllowlist)
	}
}

func TestLoadZeroAndEmptyOverrides(t *testing.T) {
	path := writeConfig(t, validEnv+`OCI_ACCELERATOR_METRICS_LISTEN=0.0.0.0:9090
OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true
OCI_ACCELERATOR_REDIRECT_ALLOWLIST=cdn.example
OCI_ACCELERATOR_CACHE_FILL_WAIT_TIMEOUT=2s
OCI_ACCELERATOR_CACHE_MUTABLE_TTL=5s
OCI_ACCELERATOR_CACHE_MUTABLE_SHARED=true
OCI_ACCELERATOR_CACHE_AUTHORIZATION_TTL=3s
OCI_ACCELERATOR_CACHE_ANONYMOUS_AUTHORIZATION_TTL=4s
`)
	values := map[string]string{
		"METRICS_LISTEN":                    "",
		"METRICS_ALLOW_REMOTE":              "false",
		"REDIRECT_ALLOWLIST":                "",
		"CACHE_FILL_WAIT_TIMEOUT":           "0s",
		"CACHE_MUTABLE_TTL":                 "0",
		"CACHE_MUTABLE_SHARED":              "false",
		"CACHE_AUTHORIZATION_TTL":           "0s",
		"CACHE_ANONYMOUS_AUTHORIZATION_TTL": "0s",
	}

	c, err := load(path, environmentLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsListen != "" || len(c.RedirectAllowlist) != 0 || c.Cache.FillWaitTimeout != 0 || c.Cache.MutableTTL != 0 || c.Cache.MutableShared || c.Cache.AuthorizationTTL != 0 || c.Cache.AnonymousAuthorizationTTL != 0 {
		t.Fatalf("zero overrides ignored: %+v", c)
	}

	delete(values, "METRICS_LISTEN")
	if _, err := load(path, environmentLookup(values)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("false override must reject remote metrics from file: %v", err)
	}
}
