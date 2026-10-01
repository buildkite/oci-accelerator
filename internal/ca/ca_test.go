//go:build unix

package ca

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestOpenRequiresExistingParent(t *testing.T) {
	for _, parent := range []string{"missing", filepath.Join("missing", "nested")} {
		t.Run(parent, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, parent, "ca")
			if _, err := Open(dir, 1); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "CA parent") {
				t.Fatalf("Open with missing parent: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("Open created entries despite missing parent: %v", entries)
			}
		})
	}
}

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

func TestNewCAUsesOCIAcceleratorName(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.cert.Subject.CommonName, "OCI Accelerator CA"; got != want {
		t.Fatalf("CA common name = %q, want %q", got, want)
	}
}

func TestOpenPreservesOldNameCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := Open(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	rewriteCertificate(t, dir, a, a.key, func(cert *x509.Certificate) {
		cert.Subject.CommonName = "Docker Accelerator CA"
		cert.RawSubject = nil
	})
	certBefore, err := os.ReadFile(filepath.Join(dir, certName))
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, 1)
	if err != nil {
		t.Fatalf("Open old-name CA: %v", err)
	}
	if reopened.cert.Subject.CommonName != "Docker Accelerator CA" {
		t.Fatalf("CA common name changed to %q", reopened.cert.Subject.CommonName)
	}
	certAfter, _ := os.ReadFile(filepath.Join(dir, certName))
	keyAfter, _ := os.ReadFile(filepath.Join(dir, keyName))
	if !bytes.Equal(certBefore, certAfter) || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("Open changed old-name CA certificate or key bytes")
	}
	roots := x509.NewCertPool()
	roots.AddCert(reopened.cert)
	leaf, err := reopened.Certificate("registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "registry.example"}); err != nil {
		t.Fatalf("verify leaf against old-name CA: %v", err)
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

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Certificate("a.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Certificate("b.example"); err != nil {
		t.Fatal(err)
	}
	// Touch "a" so "b" becomes least recently used, then overflow the cache.
	if _, err := a.Certificate("a.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Certificate("c.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.leaves["b.example"]; ok {
		t.Fatal("least recently used entry was not evicted")
	}
	for _, host := range []string{"a.example", "c.example"} {
		if _, ok := a.leaves[host]; !ok {
			t.Fatalf("recently used entry %q was evicted", host)
		}
	}
	if a.order.Len() != len(a.leaves) || len(a.leaves) != 2 {
		t.Fatalf("cache desync: order=%d leaves=%d", a.order.Len(), len(a.leaves))
	}
}

func TestRenewalRefreshesRecency(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 2)
	if err != nil {
		t.Fatal(err)
	}
	now := a.cert.NotBefore.Add(3 * time.Hour)
	a.now = func() time.Time { return now }
	x, err := a.Certificate("x.example")
	if err != nil {
		t.Fatal(err)
	}
	xDER := bytes.Clone(x.Certificate[0])
	xNotAfter := x.Leaf.NotAfter
	if _, err := a.Certificate("y.example"); err != nil {
		t.Fatal(err)
	}
	// Advance "x" to the renewal boundary. Renewal must move it to most-recently
	// used, so the next overflow evicts "y" rather than "x".
	now = x.Leaf.NotAfter.Add(-leafRenewal)
	renewed, err := a.Certificate("x.example")
	if err != nil || renewed == x {
		t.Fatalf("near-expiry leaf was not renewed: %v", err)
	}
	if !bytes.Equal(x.Certificate[0], xDER) || !x.Leaf.NotAfter.Equal(xNotAfter) {
		t.Fatal("renewal mutated the original certificate")
	}
	verifyCertificateAt(t, a, renewed, "x.example", now)
	if _, err := a.Certificate("z.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.leaves["y.example"]; ok {
		t.Fatal("renewal did not refresh recency: least recently used entry survived")
	}
	if _, ok := a.leaves["x.example"]; !ok {
		t.Fatal("renewed entry was evicted despite being most recently used")
	}
}

func TestCapacityOneRenewal(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 1)
	if err != nil {
		t.Fatal(err)
	}
	now := a.cert.NotBefore.Add(3 * time.Hour)
	a.now = func() time.Time { return now }
	first, err := a.Certificate("only.example")
	if err != nil {
		t.Fatal(err)
	}
	firstDER := bytes.Clone(first.Certificate[0])
	firstNotAfter := first.Leaf.NotAfter
	now = first.Leaf.NotAfter.Add(-leafRenewal)
	second, err := a.Certificate("only.example")
	if err != nil || second == first {
		t.Fatalf("sole entry was not renewed: %v", err)
	}
	if !bytes.Equal(first.Certificate[0], firstDER) || !first.Leaf.NotAfter.Equal(firstNotAfter) {
		t.Fatal("renewal mutated the original certificate")
	}
	verifyCertificateAt(t, a, second, "only.example", now)
	if a.order.Len() != 1 || len(a.leaves) != 1 {
		t.Fatalf("cache desync at capacity 1: order=%d leaves=%d", a.order.Len(), len(a.leaves))
	}
}

func TestCacheBackingStoreStaysBounded(t *testing.T) {
	const maxLeaves = 4
	a, err := Open(filepath.Join(t.TempDir(), "ca"), maxLeaves)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		host := fmt.Sprintf("host-%d.example", i)
		if _, err := a.Certificate(host); err != nil {
			t.Fatalf("Certificate(%q): %v", host, err)
		}
		if a.order.Len() > maxLeaves || len(a.leaves) > maxLeaves {
			t.Fatalf("cache exceeded bound: order=%d leaves=%d", a.order.Len(), len(a.leaves))
		}
		if a.order.Len() != len(a.leaves) {
			t.Fatalf("cache desync: order=%d leaves=%d", a.order.Len(), len(a.leaves))
		}
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
	now := a.cert.NotBefore.Add(3 * time.Hour)
	a.now = func() time.Time { return now }
	one, err := a.Certificate("EXAMPLE.com.")
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.Certificate("example.com")
	if err != nil || one != two {
		t.Fatalf("canonical leaf was not reused: %v", err)
	}
	oneDER := bytes.Clone(one.Certificate[0])
	oneNotAfter := one.Leaf.NotAfter
	now = one.Leaf.NotAfter.Add(-leafRenewal)
	three, err := a.Certificate("example.com")
	if err != nil || three == one {
		t.Fatalf("near-expiry leaf was not renewed: %v", err)
	}
	if !bytes.Equal(one.Certificate[0], oneDER) || !one.Leaf.NotAfter.Equal(oneNotAfter) {
		t.Fatal("renewal mutated the original certificate")
	}
	verifyCertificateAt(t, a, three, "example.com", now)
	now = a.cert.NotAfter.Add(-30 * time.Minute)
	limited, err := a.Certificate("limited.example")
	if err != nil {
		t.Fatal(err)
	}
	if limited.Leaf.NotAfter.After(a.cert.NotAfter) {
		t.Fatal("leaf outlives CA")
	}
	verifyCertificateAt(t, a, limited, "limited.example", now)
	for _, invalid := range []string{"bad..host", "-bad.example", "bad_host"} {
		if _, err := a.Certificate(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestConcurrentInitializationNeverOverwrites(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ca")
	const count = 20
	results := make(chan *Authority, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := Open(dir, 1)
			if err != nil {
				// Every concurrent opener must succeed: the winner publishes
				// the CA and losers adopt it. None may fail now that there is
				// no persistent initialization lock to contend on.
				t.Errorf("concurrent Open failed: %v", err)
			}
			results <- a
		}()
	}
	wg.Wait()
	close(results)
	var fingerprint string
	for a := range results {
		if a == nil {
			continue
		}
		if fingerprint == "" {
			fingerprint = a.Fingerprint()
		} else if a.Fingerprint() != fingerprint {
			t.Fatal("concurrent initialization rotated CA")
		}
	}
	if fingerprint == "" {
		t.Fatal("no initializer succeeded")
	}
	a, err := Open(dir, 1)
	if err != nil || a.Fingerprint() != fingerprint {
		t.Fatalf("published CA changed: %v", err)
	}
	// Losing initializers must clean up their staging directories, and no
	// initialization lock may be left behind.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "ca" {
			t.Fatalf("leftover artifact in parent: %q", e.Name())
		}
	}
}

func TestConcurrentInitializeAllSucceed(t *testing.T) {
	// Exercise the rename contention directly: Open short-circuits to load once
	// dir exists, so calling initialize from every goroutine is what forces the
	// losing-initializer adopt branch to run.
	dir := filepath.Join(t.TempDir(), "ca")
	const count = 20
	var wg sync.WaitGroup
	errs := make([]error, count)
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = initialize(dir)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("initialize %d: %v", i, err)
		}
	}
	a, err := Open(dir, 1)
	if err != nil {
		t.Fatalf("published CA is not loadable: %v", err)
	}
	if a.Fingerprint() == "" {
		t.Fatal("empty fingerprint")
	}
	// Exactly one CA is published and every loser cleaned up its staging dir.
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dir) {
		t.Fatalf("leftover artifacts in parent: %v", entries)
	}
}

func TestInitializeSurfacesRenameFailureForNonCADir(t *testing.T) {
	// A pre-existing non-empty directory that is not a published CA must make
	// initialize surface the rename failure rather than falsely adopt it. A
	// non-empty destination fails rename identically on Linux (ENOTEMPTY) and
	// macOS (EEXIST), so this exercises the surface branch portably.
	dir := filepath.Join(t.TempDir(), "ca")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "stray")
	if err := os.WriteFile(stray, []byte("not a CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initialize(dir); err == nil {
		t.Fatal("adopted a non-CA directory as a published CA")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "stray" {
		t.Fatalf("pre-existing directory was modified: %v", entries)
	}
}

func TestInitializeSurfacesForShapeValidButInvalidCA(t *testing.T) {
	// A directory whose files have CA-like names and permissions but garbage
	// contents is shape-valid, so publishedCA adopts it, but Open must still fail
	// closed because load re-validates contents. This documents that adoption
	// defers content validation to load and never yields a bad Authority.
	dir := filepath.Join(t.TempDir(), "ca")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, certName), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initialize(dir); err != nil {
		t.Fatalf("shape-valid dir should be adopted by initialize: %v", err)
	}
	if _, err := Open(dir, 1); err == nil {
		t.Fatal("Open accepted a shape-valid but invalid CA")
	}
}

func TestCertificatePermissionBounds(t *testing.T) {
	// The public certificate may be stricter than 0644 but never broader.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o600, 0o400} {
		t.Run("accept-"+mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			if _, err := Open(dir, 1); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, certName), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err != nil {
				t.Fatalf("rejected safe certificate mode %v: %v", mode, err)
			}
		})
	}
	for _, mode := range []os.FileMode{0o646, 0o664, 0o666, 0o755} {
		t.Run("reject-"+mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			if _, err := Open(dir, 1); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, certName), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil {
				t.Fatalf("accepted overly permissive certificate mode %v", mode)
			}
		})
	}
}

func TestKeyPermissionStaysStrict(t *testing.T) {
	// The private key must never grant any access beyond the owner, including
	// read, write, and execute bits for group and other.
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o610, 0o601, 0o602} {
		t.Run("reject-"+mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			if _, err := Open(dir, 1); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, keyName), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil {
				t.Fatalf("accepted key with group/world access, mode %v", mode)
			}
		})
	}
	// A key stricter than 0600 (owner read-only) is still accepted.
	t.Run("accept-0400", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ca")
		if _, err := Open(dir, 1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, keyName), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, 1); err != nil {
			t.Fatalf("rejected owner-only key mode 0400: %v", err)
		}
	})
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

func TestReadRegularSizeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pem")
	for _, size := range []int{64 << 10, (64 << 10) + 1} {
		want := bytes.Repeat([]byte("x"), size)
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readRegular(path, 0o600)
		if size == 64<<10 {
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("boundary read: bytes=%d err=%v", len(got), err)
			}
		} else if err == nil || got != nil {
			t.Fatalf("oversized read: bytes=%d err=%v", len(got), err)
		}
	}
}

func TestOpenRejectsOversizedPEMWithoutReplacement(t *testing.T) {
	for _, name := range []string{certName, keyName} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			if _, err := Open(dir, 1); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name)
			// A sparse file exercises a large on-disk size without allocating a
			// large fixture. The loader must reject it before PEM parsing.
			if err := os.Truncate(path, 1<<30); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("Open oversized %s = %v", name, err)
			}
			if st, err := os.Stat(path); err != nil || st.Size() != 1<<30 {
				t.Fatalf("rejected file replaced: %v, %v", st, err)
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
