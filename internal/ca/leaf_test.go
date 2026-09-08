package ca

import (
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"
)

func TestLeafClockRollback(t *testing.T) {
	for _, capped := range []bool{false, true} {
		name := "ordinary"
		if capped {
			name = "CA-capped"
		}
		t.Run(name, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "ca"), 1)
			if err != nil {
				t.Fatal(err)
			}
			now := a.cert.NotBefore.Add(3 * time.Hour)
			if capped {
				now = a.cert.NotAfter.Add(-30 * time.Minute)
			}
			a.now = func() time.Time { return now }
			first, err := a.Certificate("example.com")
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(-2 * time.Hour)
			replaced, err := a.Certificate("example.com")
			if err != nil {
				t.Fatal(err)
			}
			if replaced == first {
				t.Fatal("returned not-yet-valid cached leaf after clock rollback")
			}
			verifyLeafAt(t, a, replaced, now)
		})
	}
}

func TestLeafValidityBoundaries(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 1)
	if err != nil {
		t.Fatal(err)
	}
	now := a.cert.NotBefore.Add(3 * time.Hour)
	a.now = func() time.Time { return now }
	first, err := a.Certificate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{first.Leaf.NotBefore, first.Leaf.NotAfter.Add(-leafRenewal - time.Second)} {
		now = at
		got, err := a.Certificate("example.com")
		if err != nil || got != first {
			t.Fatalf("did not reuse valid leaf at %s: %v", now, err)
		}
	}
	now = first.Leaf.NotAfter.Add(-leafRenewal)
	renewed, err := a.Certificate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first || !renewed.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Fatal("leaf was not renewed at renewal boundary")
	}
	verifyLeafAt(t, a, renewed, now)
	now = renewed.Leaf.NotAfter
	replaced, err := a.Certificate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if replaced == renewed {
		t.Fatal("returned expired cached leaf")
	}
	verifyLeafAt(t, a, replaced, now)
}

func TestLeafReuseUntilCAExpiry(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 2)
	if err != nil {
		t.Fatal(err)
	}
	now := a.cert.NotAfter.Add(-30 * time.Minute)
	a.now = func() time.Time { return now }
	first, err := a.Certificate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Leaf.NotAfter.Equal(a.cert.NotAfter) {
		t.Fatal("leaf is not capped at CA expiry")
	}
	if _, err := a.Certificate("other.example"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		now = now.Add(time.Minute)
		got, err := a.Certificate("example.com")
		if err != nil || got != first {
			t.Fatalf("reissued CA-capped leaf: %v", err)
		}
		verifyLeafAt(t, a, got, now)
	}
	if _, err := a.Certificate("third.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.leaves["other.example"]; ok {
		t.Fatal("CA-capped cache hit did not refresh recency")
	}
	if _, ok := a.leaves["example.com"]; !ok {
		t.Fatal("recently used CA-capped leaf was evicted")
	}
	now = a.cert.NotAfter.Add(-time.Second)
	if got, err := a.Certificate("example.com"); err != nil || got != first {
		t.Fatalf("did not reuse leaf just before CA expiry: %v", err)
	}
	for _, at := range []time.Time{a.cert.NotBefore.Add(-time.Second), a.cert.NotAfter, a.cert.NotAfter.Add(time.Second)} {
		now = at
		for _, host := range []string{"example.com", "uncached.example"} {
			if got, err := a.Certificate(host); err == nil || got != nil {
				t.Fatalf("invalid CA returned certificate for %q at %s: %v", host, now, err)
			}
		}
	}
}

func verifyLeafAt(t *testing.T, a *Authority, leaf *tls.Certificate, now time.Time) {
	t.Helper()
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com", CurrentTime: now}); err != nil {
		t.Fatalf("verify returned certificate bytes: %v", err)
	}
}
