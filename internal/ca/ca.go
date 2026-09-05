// Package ca manages the proxy's persistent certificate authority and leaf certificates.
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
	"runtime"
	"syscall"
	"time"
)

const (
	certName = "ca.crt"
	keyName  = "ca.key"
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
func Open(dir string, maxLeaves int) (*Authority, error) {
	if dir == "" || maxLeaves <= 0 {
		return nil, errors.New("CA directory and a positive leaf limit are required")
	}
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := initialize(dir); err != nil {
			return nil, fmt.Errorf("initialize CA: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect CA directory: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("CA directory must be a non-symlink directory with mode 0700")
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
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("CA parent must be a non-symlink owner-controlled directory")
	}
	if runtime.GOOS != "windows" {
		stat, ok := parentInfo.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return errors.New("CA parent must be owned by the current user")
		}
	}
	lock := dir + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		return fmt.Errorf("acquire initialization lock (remove a stale lock only after verifying no initialization is active): %w", err)
	}
	defer os.Remove(lock)
	if _, err := os.Lstat(dir); err == nil {
		return errors.New("CA path appeared during initialization")
	} else if !errors.Is(err, os.ErrNotExist) {
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
	if _, err := os.Lstat(dir); err == nil {
		return errors.New("CA path appeared during initialization")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, dir); err != nil {
		return err
	}
	return syncDir(parent)
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
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Docker Accelerator CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
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
	if err != nil || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil {
		return nil, nil, errors.New("invalid CA certificate")
	}
	kb, rest := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" || len(rest) != 0 {
		return nil, nil, errors.New("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil || key.Curve != elliptic.P256() {
		return nil, nil, errors.New("invalid CA private key")
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

func readRegular(path string, mode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return nil, errors.New("file has insecure type or permissions")
	}
	return os.ReadFile(path)
}
