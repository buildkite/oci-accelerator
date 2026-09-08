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

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		secret string
	}{
		{name: "missing_config"},
		{name: "unexpected_argument", args: []string{"extra"}},
		{name: "unexpected_argument_after_config", args: []string{"--config", "x", "extra"}},
		{name: "unknown_flag", args: []string{"--unknown-secret-sentinel"}, secret: "unknown-secret-sentinel"},
		{name: "invalid_bool_flag", args: []string{"--check=bool-secret-sentinel"}, secret: "bool-secret-sentinel"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := run(context.Background(), test.args, &out, &errOut)
			if err == nil {
				t.Fatalf("run(%q) succeeded", test.args)
			}
			if out.Len() != 0 || errOut.Len() != 0 {
				t.Errorf("invalid arguments wrote output: stdout=%q stderr=%q", out.String(), errOut.String())
			}
			for stream, value := range map[string]string{
				"error":  err.Error(),
				"stdout": out.String(),
				"stderr": errOut.String(),
			} {
				if test.secret != "" && strings.Contains(value, test.secret) {
					t.Errorf("run(%q) leaked sensitive input in %s: %q", test.args, stream, value)
				}
			}
		})
	}
}

func TestRunValidatesConfigWithoutLeakingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	secret := "password-do-not-print"
	if err := os.WriteFile(path, []byte(`registries: [user:`+secret+`@example.com]
ca_dir: ca
cache: {dir: cache}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"--config", path}, &out, &errOut)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("unsafe result: err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestRunCheckValidatesWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(`registries: [example.com]
ca_dir: ca
cache: {dir: cache}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "--check"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration validated") {
		t.Fatalf("output: %q", out.String())
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
