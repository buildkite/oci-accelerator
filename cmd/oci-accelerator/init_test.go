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
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestRunInitCreatesUsableLayoutAndReusesCA(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runStarterCommand(t, []string{"init"})
	if err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		".env": 0o600, "data": 0o700, "data/cache": 0o700,
		"data/ca": 0o700, "data/ca/ca.crt": 0o644, "data/ca/ca.key": 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: info = %v, error = %v, want mode %o", path, info, err, mode)
		}
	}
	certPEM, err := os.ReadFile("data/ca/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile("data/ca/ca.key")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA || cert.CheckSignatureFrom(cert) != nil {
		t.Fatal("init did not generate a self-signed root CA")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))
	for _, want := range []string{"env_file=\".env\"", "ca_certificate=\"data/ca/ca.crt\"", "ca_sha256=" + fingerprint, "oci-accelerator --env-file .env", "Docker"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %s", want, out)
		}
	}

	// The generated configuration must start the real stack and retain the CA.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listener := newTestListener(t)
	t.Setenv("OCI_ACCELERATOR_LISTEN", listener.Addr().String())
	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"--env-file", ".env"}, &stdout, &stderr, listenForTest(listener)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), `"ca_sha256":"`+fingerprint+`"`) {
		t.Fatalf("startup did not reuse the initialized CA: %s", stderr.String())
	}

	// Removing only the generated config allows init to reuse existing state.
	if err := os.Remove(".env"); err != nil {
		t.Fatal(err)
	}
	if again, err := runStarterCommand(t, []string{"init"}); err != nil || again != out {
		t.Fatalf("reuse: output = %q, error = %v", again, err)
	}
	for path, want := range map[string][]byte{"data/ca/ca.crt": certPEM, "data/ca/ca.key": keyPEM} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("reuse replaced %s: %v", path, err)
		}
	}
}

func TestRunInitWithRestrictiveUmask(t *testing.T) {
	const helper = "OCI_ACCELERATOR_INIT_UMASK_HELPER"
	if os.Getenv(helper) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestRunInitWithRestrictiveUmask$")
		cmd.Env = append(os.Environ(), helper+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restrictive umask: %v\n%s", err, output)
		}
		return
	}

	t.Chdir(t.TempDir())
	previous := syscall.Umask(0o400)
	defer syscall.Umask(previous)
	if _, err := runStarterCommand(t, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{".env": 0o600, "data": 0o700, "data/cache": 0o700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode: %v, %v, want %o", path, info, err, want)
		}
	}
	if _, _, err := resolveConfig([]string{"--env-file", ".env", "--check"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunStarterCommandsIgnoreEnvironment(t *testing.T) {
	want, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"init", "init-env"} {
		t.Run(command, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("OCI_ACCELERATOR_MAX_CONNECTIONS", "invalid-secret")
			t.Setenv("OCI_ACCELERATOR_CA_DIR", "unexpected-ca")
			if _, err := runStarterCommand(t, []string{command}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(".env")
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("starter differs from example: %v", err)
			}
			if _, err := os.Lstat("unexpected-ca"); !os.IsNotExist(err) {
				t.Fatalf("used environment CA path: %v", err)
			}
			if command == "init-env" {
				entries, err := os.ReadDir(".")
				if err != nil || len(entries) != 1 {
					t.Fatalf("init-env created extra state: %v, %v", entries, err)
				}
			}
		})
	}
}

func TestRunInitEnvOutputAndNoOverwrite(t *testing.T) {
	want, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	if _, err := runStarterCommand(t, []string{"init-env", "--output", "custom.env"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("custom.env")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("custom output differs from example: %v", err)
	}

	if err := os.WriteFile("custom.env", []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runStarterCommand(t, []string{"init-env", "--output", "custom.env"}); err == nil {
		t.Fatal("overwrote existing output")
	}
	got, err = os.ReadFile("custom.env")
	if err != nil || string(got) != "keep me" {
		t.Fatalf("existing output changed: %q, %v", got, err)
	}
	if _, err := runStarterCommand(t, []string{"init-env", "--output", "missing/custom.env"}); err == nil {
		t.Fatal("accepted missing parent")
	}
	if _, err := os.Lstat("missing"); !os.IsNotExist(err) {
		t.Fatalf("created output parent: %v", err)
	}
}

func TestRunInitRefusesExistingEnvBeforeCreatingState(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var err error
			if kind == "file" {
				err = os.WriteFile(".env", []byte("keep me"), 0o600)
			} else {
				err = os.Symlink("missing", ".env")
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, err := runStarterCommand(t, []string{"init"}); err == nil || out != "" {
				t.Fatalf("accepted existing env: %q, %v", out, err)
			}
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 1 || entries[0].Name() != ".env" {
				t.Fatalf("created state: %v, %v", entries, err)
			}
			if kind == "file" {
				got, err := os.ReadFile(".env")
				if err != nil || string(got) != "keep me" {
					t.Fatalf("changed existing env: %q, %v", got, err)
				}
			} else if target, err := os.Readlink(".env"); err != nil || target != "missing" {
				t.Fatalf("changed symlink: %q, %v", target, err)
			}
		})
	}
}

func TestRunInitRejectsUnsuitableState(t *testing.T) {
	for _, kind := range []string{"public-data", "symlink-data", "file-cache", "empty-ca", "incomplete-ca"} {
		t.Run(kind, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.Mkdir("data", 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "public-data":
				err = os.Chmod("data", 0o755)
			case "symlink-data":
				if err = os.Rename("data", "target"); err == nil {
					err = os.Symlink("target", "data")
				}
			case "file-cache":
				err = os.WriteFile("data/cache", []byte("keep me"), 0o600)
			case "empty-ca", "incomplete-ca":
				err = os.Mkdir("data/ca", 0o700)
				if err == nil && kind == "incomplete-ca" {
					err = os.WriteFile("data/ca/ca.key", []byte("keep me"), 0o600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, err := runStarterCommand(t, []string{"init"}); err == nil || out != "" {
				t.Fatalf("accepted unsuitable state: %q, %v", out, err)
			}
			if _, err := os.Lstat(".env"); !os.IsNotExist(err) {
				t.Fatalf("published config after failure: %v", err)
			}
			switch kind {
			case "public-data":
				info, err := os.Stat("data")
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("changed data permissions: %v, %v", info, err)
				}
			case "symlink-data":
				entries, err := os.ReadDir("target")
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified symlink target: %v, %v", entries, err)
				}
			case "file-cache", "incomplete-ca":
				path := "data/cache"
				if kind == "incomplete-ca" {
					path = "data/ca/ca.key"
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != "keep me" {
					t.Fatalf("changed %s: %q, %v", path, got, err)
				}
			case "empty-ca":
				entries, err := os.ReadDir("data/ca")
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified incomplete CA: %v, %v", entries, err)
				}
			}
		})
	}
}

func TestRunStarterHelpAndInvalidArgumentsDoNotCreateState(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--help"}, {"init-env", "--help"},
		{"init", "extra-secret"}, {"init", "--output", "secret.env"},
		{"init-env", "extra-secret"}, {"init-env", "--output"},
		{"init-env", "--output", " \t"}, {"init-env", "--env-file", "secret.env"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Chdir(t.TempDir())
			out, err := runStarterCommand(t, args)
			if args[1] == "--help" {
				if err != nil || !strings.Contains(out, "Usage: oci-accelerator "+args[0]) {
					t.Fatalf("help: %q, %v", out, err)
				}
			} else if err == nil || out != "" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid arguments accepted or leaked: %q, %v", out, err)
			}
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 0 {
				t.Fatalf("created state: %v, %v", entries, err)
			}
		})
	}
}

func runStarterCommand(t *testing.T, args []string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, &stdout, &stderr, func(string, string) (net.Listener, error) {
		t.Fatal("initialization tried to listen")
		return nil, nil
	})
	if stderr.Len() != 0 {
		t.Fatalf("initialization logged to stderr: %s", stderr.String())
	}
	return stdout.String(), err
}
