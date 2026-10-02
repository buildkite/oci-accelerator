package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvPrecedenceAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(t.TempDir(), ".env")
	contents := "OCI_ACCELERATOR_REGISTRIES=file.example\r\nOCI_ACCELERATOR_CA_DIR='ca#literal' # note\r\nOCI_ACCELERATOR_CACHE_DIR=cache\r\nOCI_ACCELERATOR_REDIRECT_ALLOWLIST=from-file\r\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_ACCELERATOR_REGISTRIES", "process.example")
	t.Setenv("OCI_ACCELERATOR_REDIRECT_ALLOWLIST", "")
	c, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Registries[0].String() != "process.example" || len(c.RedirectAllowlist) != 0 {
		t.Fatalf("process overrides not applied: %+v", c)
	}
	assertEqual(t, "CADir", c.CADir, filepath.Join(root, "ca#literal"))
	assertEqual(t, "Cache.Dir", c.Cache.Dir, filepath.Join(root, "cache"))
}

func TestReadEnvFileSyntaxAndSafety(t *testing.T) {
	if values, err := ReadEnvFile(""); err != nil || len(values) != 0 {
		t.Fatalf("ReadEnvFile empty = %v, %v", values, err)
	}
	for _, test := range []struct{ name, contents string }{
		{"unknown", "SECRET_VALUE=x\n"},
		{"duplicate", "OCI_ACCELERATOR_LISTEN=x\nOCI_ACCELERATOR_LISTEN=y\n"},
		{"bare", "OCI_ACCELERATOR_LISTEN\n"},
		{"export", "export OCI_ACCELERATOR_LISTEN=x\n"},
		{"quote", "OCI_ACCELERATOR_LISTEN='x\n"},
		{"trailing", "OCI_ACCELERATOR_LISTEN='x' junk\n"},
		{"nul", "OCI_ACCELERATOR_LISTEN=x\x00y\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.contents)
			_, err := ReadEnvFile(path)
			if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "SECRET_VALUE") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadEnvParsesOnlyEffectiveValuesAfterFileSyntax(t *testing.T) {
	path := writeConfig(t, "OCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\nOCI_ACCELERATOR_MAX_CONNECTIONS=2\n")
	t.Setenv("OCI_ACCELERATOR_MAX_CONNECTIONS", "3")
	_, err := LoadEnv(path)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("override hid file error: %v", err)
	}

	path = writeConfig(t, "OCI_ACCELERATOR_REGISTRIES=file.example\nOCI_ACCELERATOR_CA_DIR=ca\nOCI_ACCELERATOR_CACHE_DIR=cache\nOCI_ACCELERATOR_MAX_CONNECTIONS=not-a-number\n")
	c, err := LoadEnv(path)
	if err != nil {
		t.Fatalf("effective process value did not override invalid file value: %v", err)
	}
	assertEqual(t, "MaxConnections", c.MaxConnections, 3)
}

func TestReadEnvFileTreatsQuotedValuesLiterally(t *testing.T) {
	path := writeConfig(t, "# comment\n\nOCI_ACCELERATOR_LISTEN=host#port\nOCI_ACCELERATOR_CA_DIR=  \"$HOME # literal\"\nOCI_ACCELERATOR_METRICS_LISTEN= # disabled\nOCI_ACCELERATOR_CACHE_DIR='$(touch never-created)'\n")
	values, err := ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "unquoted hash", values["OCI_ACCELERATOR_LISTEN"], "host#port")
	assertEqual(t, "quoted literal", values["OCI_ACCELERATOR_CA_DIR"], "$HOME # literal")
	assertEqual(t, "empty with comment", values["OCI_ACCELERATOR_METRICS_LISTEN"], "")
	assertEqual(t, "no command substitution", values["OCI_ACCELERATOR_CACHE_DIR"], "$(touch never-created)")
}

func TestReadEnvFileIOAndLongLine(t *testing.T) {
	_, err := ReadEnvFile(filepath.Join(t.TempDir(), "missing"))
	if err == nil || errors.Is(err, ErrInvalid) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
	path := writeConfig(t, "OCI_ACCELERATOR_LISTEN="+strings.Repeat("x", maxEnvLineBytes)+"\n")
	_, err = ReadEnvFile(path)
	if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), strings.Repeat("x", 20)) {
		t.Fatalf("long line error = %v", err)
	}
}

func TestLoadEnvDoesNotMutateEnvironmentOrDiscoverFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".env", []byte("OCI_ACCELERATOR_REGISTRIES=file.example\nOCI_ACCELERATOR_CA_DIR=ca\nOCI_ACCELERATOR_CACHE_DIR=cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := os.Environ()
	if _, err := LoadEnv(".env"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(before, "\x00") != strings.Join(os.Environ(), "\x00") {
		t.Fatal("LoadEnv mutated process environment")
	}
	_, err := LoadEnv("")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "registries") {
		t.Fatalf("dotenv was discovered: %v", err)
	}
}
