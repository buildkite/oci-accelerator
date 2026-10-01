package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitCACreatesAndReusesRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	args := []string{"init-ca", "--dir", "ca"}
	out, err := runCACommand(t, args)
	if err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile("ca/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile("ca/ca.key")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certBefore, keyBefore)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA || len(cert.DNSNames) != 0 || len(cert.IPAddresses) != 0 {
		t.Fatal("expected root CA, not a hostname certificate")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("root is not self-signed: %v", err)
	}
	want := fmt.Sprintf("ca_certificate=%q\nca_sha256=%x\n", filepath.Join("ca", "ca.crt"), sha256.Sum256(cert.Raw))
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	for path, mode := range map[string]os.FileMode{"ca": 0o700, "ca/ca.crt": 0o644, "ca/ca.key": 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	if again, err := runCACommand(t, args); err != nil || again != out {
		t.Fatalf("reuse: output = %q, error = %v", again, err)
	}
	for path, before := range map[string][]byte{"ca/ca.crt": certBefore, "ca/ca.key": keyBefore} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("reuse changed %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "ca" {
		t.Fatalf("unexpected state outside CA: %v, %v", entries, err)
	}
	entries, err = os.ReadDir("ca")
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected only certificate and key: %v, %v", entries, err)
	}
}

func TestRunInitCAUsesEnvironmentDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")

	out, err := runCACommand(t, []string{"init-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `ca_certificate="env-ca/ca.crt"`) {
		t.Fatalf("output = %q", out)
	}
	if _, err := os.Stat("env-ca/ca.key"); err != nil {
		t.Fatalf("environment CA directory was not used: %v", err)
	}
}

func TestRunInitCAExplicitDirectoryOverridesEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")

	if _, err := runCACommand(t, []string{"init-ca", "--dir", "flag-ca"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("flag-ca/ca.key"); err != nil {
		t.Fatalf("explicit CA directory was not used: %v", err)
	}
	if _, err := os.Stat("env-ca"); !os.IsNotExist(err) {
		t.Fatalf("environment CA directory was used: %v", err)
	}
}

func TestRunInitCAExplicitEmptyDirectoryRejectsEnvironmentFallback(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("OCI_ACCELERATOR_CA_DIR", "env-ca")

	out, err := runCACommand(t, []string{"init-ca", "--dir="})
	if err == nil || out != "" {
		t.Fatalf("explicit empty directory: output = %q, error = %v", out, err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("explicit empty directory created state: %v, %v", entries, readErr)
	}
}

func TestRunInitCAHelpAndInvalidArgumentsDoNotCreateState(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"--help"},
		{"--dir", "ca", "--help"},
		{},
		{"--dir"},
		{"--dir", ""},
		{"--dir", " \t"},
		{"--dir", "ca", "extra-secret"},
		{"--dir", "ca", "--unknown-secret"},
		{"--dir", "ca", "--config", "secret.yaml"},
		{"--dir", "ca", "--check"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			out, err := runCACommand(t, append([]string{"init-ca"}, args...))
			help := len(args) > 0 && args[len(args)-1] == "--help"
			if help {
				if err != nil || !strings.Contains(out, "Usage: oci-accelerator init-ca [--dir DIR]") {
					t.Fatalf("help: output = %q, error = %v", out, err)
				}
			} else if err == nil || out != "" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid arguments: output = %q, error = %v", out, err)
			}
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 0 {
				t.Fatalf("help/invalid command created state: %v, %v", entries, err)
			}
		})
	}
}

func TestRunInitCARejectsExistingInvalidStateWithoutReplacingIt(t *testing.T) {
	for _, damage := range []string{"empty", "certificate", "key", "permissions"} {
		t.Run(damage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			args := []string{"init-ca", "--dir", dir}
			if _, err := runCACommand(t, args); err != nil {
				t.Fatal(err)
			}
			certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
			var err error
			switch damage {
			case "empty":
				if err = os.Remove(certPath); err == nil {
					err = os.Remove(keyPath)
				}
			case "certificate":
				err = os.WriteFile(certPath, []byte("invalid-secret-certificate"), 0o644)
			case "key":
				err = os.Remove(keyPath)
			case "permissions":
				err = os.Chmod(keyPath, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			certBefore, certErr := os.ReadFile(certPath)
			keyBefore, keyErr := os.ReadFile(keyPath)
			out, err := runCACommand(t, args)
			if err == nil || out != "" || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe state accepted or leaked: output = %q, error = %v", out, err)
			}
			for path, before := range map[string][]byte{certPath: certBefore, keyPath: keyBefore} {
				after, err := os.ReadFile(path)
				wasMissing := path == certPath && os.IsNotExist(certErr) || path == keyPath && os.IsNotExist(keyErr)
				if wasMissing {
					if !os.IsNotExist(err) {
						t.Fatalf("recreated %s: %v", path, err)
					}
				} else if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("changed %s: %v", path, err)
				}
			}
		})
	}
}

func TestRunInitCARequiresExistingParent(t *testing.T) {
	root := t.TempDir()
	out, err := runCACommand(t, []string{"init-ca", "--dir", filepath.Join(root, "missing-secret", "ca")})
	if err == nil || out != "" || strings.Contains(err.Error(), "missing-secret") {
		t.Fatalf("missing parent: output = %q, error = %v", out, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("created missing parent: %v, %v", entries, err)
	}
}

func runCACommand(t *testing.T, args []string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(context.Background(), args, &out, &errOut, func(string, string) (net.Listener, error) {
		t.Fatal("CA command tried to start a listener")
		return nil, nil
	})
	if errOut.Len() != 0 {
		t.Fatalf("CA command wrote stderr: %q", errOut.String())
	}
	return out.String(), err
}
