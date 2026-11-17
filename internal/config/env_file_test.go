package config

import (
	"path/filepath"
	"testing"
)

func TestLoadPrecedenceAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := writeConfig(t, "OCI_ACCELERATOR_REGISTRIES=file.example\r\nOCI_ACCELERATOR_CA_DIR='ca#literal' # note\r\nOCI_ACCELERATOR_CACHE_DIR=cache\r\nOCI_ACCELERATOR_REDIRECT_ALLOWLIST=from-file\r\n")
	t.Setenv("OCI_ACCELERATOR_REGISTRIES", "process.example")
	t.Setenv("OCI_ACCELERATOR_REDIRECT_ALLOWLIST", "")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if c.Registries[0].String() != "process.example" || len(c.RedirectAllowlist) != 0 {
		t.Fatalf("process overrides not applied: %+v", c)
	}
	assertEqual(t, "CADir", c.CADir, filepath.Join(root, "ca#literal"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(root, "cache"))
}
