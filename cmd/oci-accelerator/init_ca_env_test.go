package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunInitCAEnvFilePrecedence(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(t.TempDir(), "ca.env")
	// Other proxy settings need not be supplied or semantically valid for init-ca.
	if err := os.WriteFile(path, []byte("OCI_ACCELERATOR_CA_DIR=file-ca\nOCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"init-ca", "--env-file", path}
	if _, err := runCACommand(t, args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("file-ca/ca.key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")
	if _, err := runCACommand(t, args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("env-ca/ca.key"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCACommand(t, append(args, "--dir", "flag-ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("flag-ca/ca.key"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCACommand(t, append(args, "--dir=")); err == nil {
		t.Fatal("empty flag fell back to environment")
	}
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "")
	if _, err := runCACommand(t, args); err == nil {
		t.Fatal("empty environment fell back to file")
	}
}
