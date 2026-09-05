package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/authority"
)

const leafRenewal = time.Hour

type leafCache struct {
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	order  []string
}

func newLeafCache() leafCache { return leafCache{leaves: make(map[string]*tls.Certificate)} }

// Certificate returns a currently valid certificate for a DNS name or IP address.
func (a *Authority) Certificate(host string) (*tls.Certificate, error) {
	now := time.Now()
	if now.Before(a.cert.NotBefore) || !now.Before(a.cert.NotAfter) {
		return nil, errors.New("CA certificate is not currently valid")
	}
	parsed, err := authority.Parse(host, "443")
	if err != nil || parsed.Port() != "443" {
		return nil, errors.New("invalid certificate host")
	}
	host = parsed.Host()
	a.mu.Lock()
	defer a.mu.Unlock()
	if cached := a.leaves[host]; cached != nil && cached.Leaf != nil && cached.Leaf.NotAfter.Sub(now) > leafRenewal {
		return cached, nil
	}
	leaf, err := a.makeLeaf(host, now)
	if err != nil {
		return nil, err
	}
	if _, exists := a.leaves[host]; !exists {
		if len(a.leaves) == a.maxLeaves {
			delete(a.leaves, a.order[0])
			a.order = a.order[1:]
		}
		a.order = append(a.order, host)
	}
	a.leaves[host] = leaf
	return leaf, nil
}

func (a *Authority) makeLeaf(host string, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(24 * time.Hour)
	if notAfter.After(a.cert.NotAfter) {
		notAfter = a.cert.NotAfter
	}
	if !notAfter.After(now) {
		return nil, errors.New("CA expires too soon to issue a leaf")
	}
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-time.Minute), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		t.IPAddresses = []net.IP{ip}
	} else {
		t.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return nil, err
	}
	pair.Leaf, err = x509.ParseCertificate(der)
	return &pair, err
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
