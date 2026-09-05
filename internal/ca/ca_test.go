package ca

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRestrictiveUmask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	previous := syscall.Umask(0o777)
	defer syscall.Umask(previous)
	if _, err := Open(dir, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, 1); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPersistsCAAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := Open(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := a.Fingerprint()
	a2, err := Open(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Fingerprint() != fingerprint {
		t.Fatal("CA rotated across restart")
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, keyName): 0o600, filepath.Join(dir, certName): 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v", path, info.Mode().Perm())
		}
	}
	if a.CertificatePath() != filepath.Join(dir, certName) {
		t.Fatal("unexpected certificate path")
	}
}

func TestLeavesVerifyAndCacheIsBounded(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 2)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.cert)
	for _, host := range []string{"registry.example", "127.0.0.1", "2001:db8::1"} {
		leaf, err := a.Certificate(host)
		if err != nil {
			t.Fatalf("Certificate(%q): %v", host, err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Fatalf("verify %q: %v", host, err)
		}
		if leaf.Leaf.NotAfter.After(a.cert.NotAfter) || leaf.Leaf.NotAfter.Sub(time.Now()) > 24*time.Hour+time.Minute {
			t.Fatal("leaf validity out of bounds")
		}
	}
	if len(a.leaves) != 2 {
		t.Fatalf("cache size = %d", len(a.leaves))
	}
}

func TestCertificateConcurrent(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 4)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Certificate("registry.example"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(a.leaves) != 1 {
		t.Fatalf("cache size = %d", len(a.leaves))
	}
}

func TestLeafReuseRenewalAndCALimit(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 2)
	if err != nil {
		t.Fatal(err)
	}
	one, err := a.Certificate("EXAMPLE.com.")
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.Certificate("example.com")
	if err != nil || one != two {
		t.Fatalf("canonical leaf was not reused: %v", err)
	}
	one.Leaf.NotAfter = time.Now().Add(leafRenewal)
	three, err := a.Certificate("example.com")
	if err != nil || three == one {
		t.Fatalf("near-expiry leaf was not renewed: %v", err)
	}
	a.cert.NotAfter = time.Now().Add(30 * time.Minute)
	limited, err := a.makeLeaf("limited.example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if limited.Leaf.NotAfter.After(a.cert.NotAfter) {
		t.Fatal("leaf outlives CA")
	}
	for _, invalid := range []string{"bad..host", "-bad.example", "bad_host"} {
		if _, err := a.Certificate(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestConcurrentInitializationNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	const count = 20
	results := make(chan *Authority, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a, _ := Open(dir, 1); results <- a }()
	}
	wg.Wait()
	close(results)
	var fingerprint string
	for a := range results {
		if a != nil {
			if fingerprint == "" {
				fingerprint = a.Fingerprint()
			} else if a.Fingerprint() != fingerprint {
				t.Fatal("concurrent initialization rotated CA")
			}
		}
	}
	if fingerprint == "" {
		t.Fatal("no initializer succeeded")
	}
	a, err := Open(dir, 1)
	if err != nil || a.Fingerprint() != fingerprint {
		t.Fatalf("published CA changed: %v", err)
	}
}

func TestExistingBadStatesFailWithoutWrites(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ca")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, 1); err == nil {
			t.Fatal("accepted empty directory")
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatal("bad state was modified")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "ca")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(link, 1); err == nil {
			t.Fatal("accepted symlink")
		}
	})
	t.Run("insecure-key", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ca")
		if _, err := Open(dir, 1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, keyName), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, 1); err == nil {
			t.Fatal("accepted insecure key")
		}
	})
}

func TestRejectsExpiredAndFutureCA(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after time.Time
	}{
		{"expired", time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour)},
		{"future", time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			a, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			tmpl := *a.cert
			tmpl.NotBefore, tmpl.NotAfter = tc.before, tc.after
			der, err := x509.CreateCertificate(nil, &tmpl, &tmpl, &a.key.PublicKey, a.key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, certName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil {
				t.Fatal("accepted invalid validity")
			}
		})
	}
}

func TestMalformedInstallationsFailWithoutReplacement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *Authority)
	}{
		{"missing-cert", func(t *testing.T, dir string, _ *Authority) {
			if err := os.Remove(filepath.Join(dir, certName)); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink-cert", func(t *testing.T, dir string, _ *Authority) {
			p := filepath.Join(dir, certName)
			data, _ := os.ReadFile(p)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "cert")
			if err := os.WriteFile(target, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"swapped-key", func(t *testing.T, dir string, _ *Authority) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			der, _ := x509.MarshalECPrivateKey(key)
			if err := os.WriteFile(filepath.Join(dir, keyName), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"no-cert-sign", func(t *testing.T, dir string, a *Authority) {
			rewriteCertificate(t, dir, a, a.key, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature })
		}},
		{"not-self-signed", func(t *testing.T, dir string, a *Authority) {
			signer, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			rewriteCertificate(t, dir, a, signer, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			a, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, dir, a)
			beforeCert, _ := os.ReadFile(filepath.Join(dir, certName))
			beforeKey, _ := os.ReadFile(filepath.Join(dir, keyName))
			if _, err := Open(dir, 1); err == nil {
				t.Fatal("accepted malformed installation")
			}
			afterCert, _ := os.ReadFile(filepath.Join(dir, certName))
			afterKey, _ := os.ReadFile(filepath.Join(dir, keyName))
			if !bytes.Equal(beforeCert, afterCert) || !bytes.Equal(beforeKey, afterKey) {
				t.Fatal("failed load replaced files")
			}
		})
	}
}

func rewriteCertificate(t *testing.T, dir string, a *Authority, signer *ecdsa.PrivateKey, mutate func(*x509.Certificate)) {
	t.Helper()
	tmpl := *a.cert
	if mutate != nil {
		mutate(&tmpl)
	}
	parent := tmpl
	parent.PublicKey = &signer.PublicKey
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &parent, &a.key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, certName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}
