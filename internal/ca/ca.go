//go:build unix

// Package ca manages the proxy's persistent certificate authority and leaf
// certificates on Unix systems.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	certName   = "ca.crt"
	keyName    = "ca.key"
	caKeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
)

// Authority is a persistent CA with a bounded in-memory leaf cache.
type Authority struct {
	dir       string
	cert      *x509.Certificate
	key       *ecdsa.PrivateKey
	maxLeaves int

	leafCache
}

// Open loads the CA in dir, creating it only when dir does not exist.
// Its parent directory must already exist; the operator is responsible for
// provisioning that parent and ensuring its durability before calling Open.
func Open(dir string, maxLeaves int) (*Authority, error) {
	if dir == "" || maxLeaves <= 0 {
		return nil, errors.New("CA directory and a positive leaf limit are required")
	}
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := initialize(dir); err != nil {
			return nil, fmt.Errorf("initialize CA: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("inspect CA directory: %w", err)
	default:
		if err := checkCADir(info); err != nil {
			return nil, err
		}
	}
	if err := checkParent(filepath.Dir(dir)); err != nil {
		return nil, err
	}

	cert, key, err := load(dir)
	if err != nil {
		return nil, err
	}
	return &Authority{dir: dir, cert: cert, key: key, maxLeaves: maxLeaves, leafCache: newLeafCache()}, nil
}

// CertificatePath returns the public CA certificate path.
func (a *Authority) CertificatePath() string { return filepath.Join(a.dir, certName) }

// Fingerprint returns the CA certificate's SHA-256 fingerprint as lowercase hex.
func (a *Authority) Fingerprint() string {
	sum := sha256.Sum256(a.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func initialize(dir string) error {
	parent := filepath.Dir(dir)
	if err := checkParent(parent); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".ca-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	certDER, key, err := newCA(time.Now())
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(stage, keyName), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(stage, certName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o644); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	// Publish atomically. Concurrent initializers each stage their own CA and
	// race this rename. A published CA makes the destination non-empty, so a
	// competing rename fails rather than replacing it. Publication never exposes
	// a partial CA and needs no persistent lock. A crash or SIGKILL can bypass
	// deferred cleanup and leave a private staging directory behind; see README.md
	// for manual cleanup while all initializers are stopped.
	if err := os.Rename(stage, dir); err != nil {
		// A losing initializer adopts the winner's CA. Rename can also fail for
		// reasons unrelated to a competitor (permission, I/O, cross-device), so
		// only treat the error as a lost race when dir is now a complete,
		// correctly permissioned installation; otherwise surface the failure.
		// Content validity is deliberately deferred to the caller's load, which
		// re-parses the certificate and key and fails closed on bad material.
		if publishedCA(dir) {
			return syncDir(parent)
		}
		return err
	}
	return syncDir(parent)
}

// publishedCA reports whether dir already holds both CA files with the on-disk
// types, permissions, and ownership Open requires, so a losing initializer can
// adopt an already-published CA instead of masking an unrelated rename failure.
// It checks filesystem metadata only; the caller's load validates contents.
func publishedCA(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return false
	}
	if err := checkRegular(filepath.Join(dir, certName), 0o644); err != nil {
		return false
	}
	return checkRegular(filepath.Join(dir, keyName), 0o600) == nil
}

// checkCADir verifies that an existing CA directory is a non-symlink directory
// with mode 0700 owned by the current user.
func checkCADir(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("CA directory must be a non-symlink directory with mode 0700")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("CA directory must be owned by the current user")
	}
	return nil
}

func checkParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect CA parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return errors.New("CA parent must be a non-symlink owner-controlled directory")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("CA parent must be owned by the current user")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func newCA(now time.Time) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	t := &x509.Certificate{
		SerialNumber:          serial,
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		Subject:               pkix.Name{CommonName: "OCI Accelerator CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              caKeyUsage,
	}
	der, err := x509.CreateCertificate(rand.Reader, t, t, &key.PublicKey, key)
	return der, key, err
}

func load(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := readRegular(filepath.Join(dir, certName), 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA certificate: %w", err)
	}
	keyPEM, err := readRegular(filepath.Join(dir, keyName), 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	cb, rest := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" || len(rest) != 0 {
		return nil, nil, errors.New("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if err := validateCAProfile(cert); err != nil {
		return nil, nil, err
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("verify CA self-signature: %w", err)
	}
	kb, rest := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" || len(rest) != 0 {
		return nil, nil, errors.New("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA private key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, nil, errors.New("CA private key must use P-256")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("CA certificate and key do not match")
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, errors.New("CA certificate is not currently valid")
	}
	return cert, key, nil
}

// readRegular reads path only when it is a regular file owned by the current
// user whose permission bits grant no more access than maxPerm. Bits set in
// maxPerm are permitted but not required, so a stricter mode (for example an
// owner-only public certificate) is accepted while any broader access, such as
// group- or world-write on the key, is rejected.
func readRegular(path string, maxPerm os.FileMode) ([]byte, error) {
	if err := checkRegular(path, maxPerm); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func checkRegular(path string, maxPerm os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&^maxPerm != 0 {
		return errors.New("file has insecure type or permissions")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("file must be owned by the current user")
	}
	return nil
}
