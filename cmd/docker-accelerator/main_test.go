package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"extra"}, {"--config", "x", "extra"}, {"--bad", "secret"}} {
		var errOut bytes.Buffer
		if err := run(args, &bytes.Buffer{}, &errOut); err == nil {
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
	err := run([]string{"--config", path}, &out, &errOut)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("unsafe result: %v %q", err, errOut.String())
	}
}

func TestRunSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("registries: [example.com]\nca_dir: ca\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"--config", path}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration validated") {
		t.Fatalf("output: %q", out.String())
	}
}
