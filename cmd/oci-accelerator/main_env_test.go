package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Clear only application settings before tests inject their own overrides.
	// Other variables, including subprocess controls, must remain available.
	contents, err := os.ReadFile("../../.env.example")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for _, line := range strings.Split(string(contents), "\n") {
		name, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(name, "OCI_ACCELERATOR_") {
			continue
		}

		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

func TestResolveConfigEnvFile(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(t.TempDir(), "proxy.env")
	if err := os.WriteFile(path, []byte("OCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=ca\nOCI_ACCELERATOR_CACHE_DIR=cache\nOCI_ACCELERATOR_MAX_CONNECTIONS=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_ACCELERATOR_MAX_CONNECTIONS", "7")
	var out bytes.Buffer
	cfg, handled, err := resolveConfig([]string{"--env-file", path, "--check"}, &out)
	if err != nil || !handled || out.String() != "configuration validated\n" {
		t.Fatalf("check = %t, %q, %v", handled, out.String(), err)
	}
	if cfg.MaxConnections != 7 || cfg.CADir != filepath.Join(root, "ca") || cfg.Cache.Dir != filepath.Join(root, "cache") {
		t.Fatalf("effective configuration = %+v", cfg)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("check created state: %v, %v", entries, err)
	}
	for _, action := range []string{"--help", "--version"} {
		if _, handled, err := resolveConfig([]string{"--env-file", "missing-secret", action}, &bytes.Buffer{}); err != nil || !handled {
			t.Fatalf("%s did not bypass configuration: %v", action, err)
		}
	}
	if _, _, err := resolveConfig([]string{"--env-file", "missing-secret"}, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "missing-secret") {
		t.Fatalf("missing file accepted or leaked: %v", err)
	}
}
