package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"extra"}, {"--config", "x", "extra"}, {"--bad", "secret"}} {
		var errOut bytes.Buffer
		if err := run(context.Background(), args, &bytes.Buffer{}, &errOut); err == nil {
			t.Errorf("run(%q) succeeded", args)
		}
		if errOut.Len() != 0 {
			t.Errorf("run(%q) reflected command-line input: %q", args, errOut.String())
		}
	}
}

func TestRunValidatesConfigWithoutLeakingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	secret := "password-do-not-print"
	if err := os.WriteFile(path, []byte("registries: [user:"+secret+"@example.com]\nca_dir: ca\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"--config", path}, &out, &errOut)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("unsafe result: %v %q", err, errOut.String())
	}
}

func TestRunSuccess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: ca\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "--check"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration validated") {
		t.Fatalf("output: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "ca")); !os.IsNotExist(err) {
		t.Fatalf("--check created CA directory: %v", err)
	}
}

func TestRunCheckWithCacheHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: ca\ncache:\n  dir: cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--config", path, "--check"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("--check created %s directory: %v", name, err)
		}
	}
}

func TestRunRejectsUnusableCacheBeforeListen(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "unsafe-cache")
	if err := os.Mkdir(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	config := "listen: " + occupied.Addr().String() + "\nregistries: [example.com]\nca_dir: ca\ncache:\n  dir: unsafe-cache\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	err = run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &errOut)
	if err == nil || err.Error() != "cache startup failed" {
		t.Fatalf("error = %v, want generic cache startup failure", err)
	}
	if strings.Contains(err.Error(), cacheDir) || strings.Contains(errOut.String(), cacheDir) {
		t.Fatalf("cache path leaked: %v %q", err, errOut.String())
	}
}
