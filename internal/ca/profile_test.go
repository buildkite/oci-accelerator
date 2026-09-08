//go:build unix

package ca

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratedCAProfileLoadsAndIssuesVerifiableLeaf(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	first, err := Open(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	certBytes, err := os.ReadFile(filepath.Join(dir, certName))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := Open(dir, 1)
	if err != nil {
		t.Fatalf("load generated CA: %v", err)
	}
	if first.Fingerprint() != loaded.Fingerprint() {
		t.Fatal("loaded a different CA")
	}
	leaf, err := loaded.Certificate("profile.example")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse returned leaf DER: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(loaded.cert)
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: roots, DNSName: "profile.example"}); err != nil {
		t.Fatalf("verify returned leaf DER: %v", err)
	}
	assertFileBytes(t, dir, certBytes, keyBytes)
}

func TestLoadRejectsUnsupportedCAProfiles(t *testing.T) {
	unknown := pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4, 5}, Value: []byte{0x05, 0x00}}
	tests := []struct {
		name string
		want string
		edit func(*testing.T, *x509.Certificate)
	}{
		{"client authentication usage", "unsupported CA extension", func(t *testing.T, c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}},
		{"server authentication usage", "unsupported CA extension", func(t *testing.T, c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}},
		{"permitted DNS constraint", "unsupported CA extension", func(t *testing.T, c *x509.Certificate) {
			c.PermittedDNSDomainsCritical = true
			c.PermittedDNSDomains = []string{"example.com"}
		}},
		{"excluded DNS constraint", "unsupported CA extension", func(t *testing.T, c *x509.Certificate) { c.ExcludedDNSDomains = []string{"example.com"} }},
		{"unknown critical extension", "unhandled critical extensions", func(t *testing.T, c *x509.Certificate) {
			e := unknown
			e.Critical = true
			c.ExtraExtensions = append(c.ExtraExtensions, e)
		}},
		{"unknown noncritical extension", "unsupported CA extension", func(t *testing.T, c *x509.Certificate) { c.ExtraExtensions = append(c.ExtraExtensions, unknown) }},
		{"extra key usage", "CA key usage", func(t *testing.T, c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageKeyEncipherment }},
		{"missing key usage", "CA key usage", func(t *testing.T, c *x509.Certificate) { c.KeyUsage &^= x509.KeyUsageCRLSign }},
		{"non CA", "CA basic constraints", func(t *testing.T, c *x509.Certificate) { c.IsCA = false }},
		{"missing basic constraints", "CA basic constraints", func(t *testing.T, c *x509.Certificate) { c.BasicConstraintsValid = false }},
		{"positive path length", "CA path length", func(t *testing.T, c *x509.Certificate) { c.MaxPathLen = 1 }},
		{"zero path length", "CA path length", func(t *testing.T, c *x509.Certificate) { c.MaxPathLen = 0; c.MaxPathLenZero = true }},
		{"SHA384 signature", "CA signature algorithm", func(t *testing.T, c *x509.Certificate) { c.SignatureAlgorithm = x509.ECDSAWithSHA384 }},
		{"P384 public key", "CA public key", func(t *testing.T, c *x509.Certificate) {}},
		{"not self issued", "CA certificate must be self-issued", func(t *testing.T, c *x509.Certificate) { c.Subject.CommonName = "different subject" }},
		{"key usage noncritical", "CA extension", func(t *testing.T, c *x509.Certificate) {
			overrideCriticality(t, c, asn1.ObjectIdentifier{2, 5, 29, 15}, false)
		}},
		{"basic constraints noncritical", "CA extension", func(t *testing.T, c *x509.Certificate) {
			overrideCriticality(t, c, asn1.ObjectIdentifier{2, 5, 29, 19}, false)
		}},
		{"subject key identifier critical", "subject key identifier incorrectly marked critical", func(t *testing.T, c *x509.Certificate) {
			overrideCriticality(t, c, asn1.ObjectIdentifier{2, 5, 29, 14}, true)
		}},
		{"short subject key identifier", "CA subject key identifier", func(t *testing.T, c *x509.Certificate) { c.SubjectKeyId = []byte{1, 2, 3} }},
		{"missing subject key identifier", "must contain key usage, basic constraints, and subject key identifier", func(t *testing.T, c *x509.Certificate) {
			// Suppress automatic SKI generation, but encode CA=true in the actual
			// basic constraints so only the missing extension makes it invalid.
			c.IsCA = false
			c.ExtraExtensions = []pkix.Extension{{
				Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true,
				Value: []byte{0x30, 0x03, 0x01, 0x01, 0xff},
			}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			curve := elliptic.P256()
			if tc.name == "P384 public key" {
				curve = elliptic.P384()
			}
			key, err := ecdsa.GenerateKey(curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			tmpl := profileTemplate()
			tc.edit(t, tmpl)
			parent := tmpl
			if tc.name == "not self issued" {
				parent = profileTemplate()
			}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			assertRejectedFixture(t, der, key, tc.want)
		})
	}
}

func TestLoadPreservesParseAndSignatureErrors(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, profileTemplate(), profileTemplate(), &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("certificate DER", func(t *testing.T) {
		err := openFixture(t, []byte{1, 2, 3}, marshalKey(t, key))
		assertWrappedError(t, err, "parse CA certificate")
	})
	t.Run("private key DER", func(t *testing.T) {
		err := openFixture(t, der, []byte{1, 2, 3})
		assertWrappedError(t, err, "parse CA private key")
	})
	t.Run("P384 private key", func(t *testing.T) {
		wrong, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		err = openFixture(t, der, marshalKey(t, wrong))
		if err == nil || !strings.Contains(err.Error(), "CA private key must use P-256") {
			t.Fatalf("Open error = %v", err)
		}
	})
	t.Run("bad self signature", func(t *testing.T) {
		corrupt := bytes.Clone(der)
		corrupt[len(corrupt)-1] ^= 1
		err := openFixture(t, corrupt, marshalKey(t, key))
		assertWrappedError(t, err, "verify CA self-signature")
	})
}

func profileTemplate() *x509.Certificate {
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "fixture CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              caKeyUsage,
	}
}

func overrideCriticality(t *testing.T, c *x509.Certificate, oid asn1.ObjectIdentifier, critical bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range parsed.Extensions {
		if extension.Id.Equal(oid) {
			extension.Critical = critical
			c.ExtraExtensions = append(c.ExtraExtensions, extension)
			return
		}
	}
	t.Fatalf("fixture has no extension %s", oid)
}

func assertRejectedFixture(t *testing.T, certDER []byte, key *ecdsa.PrivateKey, want string) {
	t.Helper()
	keyDER := marshalKey(t, key)
	err := openFixture(t, certDER, keyDER)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Open error = %v, want snippet %q", err, want)
	}
}

func openFixture(t *testing.T, certDER, keyDER []byte) error {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ca")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, certName), certBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyName), keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, 1)
	if err == nil || a != nil {
		t.Fatalf("Open returned authority=%v, error=%v", a, err)
	}
	assertFileBytes(t, dir, certBytes, keyBytes)
	return err
}

func assertFileBytes(t *testing.T, dir string, wantCert, wantKey []byte) {
	t.Helper()
	gotCert, err := os.ReadFile(filepath.Join(dir, certName))
	if err != nil {
		t.Fatal(err)
	}
	gotKey, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCert, wantCert) || !bytes.Equal(gotKey, wantKey) {
		t.Fatal("Open changed rejected fixture bytes")
	}
}

func marshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func assertWrappedError(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), prefix) {
		t.Fatalf("Open error = %v, want %q", err, prefix)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("%q error did not preserve underlying error", prefix)
	}
}
