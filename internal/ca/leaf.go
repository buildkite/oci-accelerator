//go:build unix

package ca

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

const leafRenewal = time.Hour

// leafEntry is stored as the Value of each list element so eviction can locate
// its map key without a reverse lookup.
type leafEntry struct {
	host string
	cert *tls.Certificate
}

// leafCache is a bounded least-recently-used cache of per-host leaf
// certificates. order.Front() is the least recently used entry and
// order.Back() the most recently used; leaves maps each host to its element.
type leafCache struct {
	mu     sync.Mutex
	leaves map[string]*list.Element
	order  *list.List
	now    func() time.Time
}

func newLeafCache() leafCache {
	return leafCache{leaves: make(map[string]*list.Element), order: list.New(), now: time.Now}
}

// Certificate returns a currently valid certificate for a DNS name or IP address.
// The returned certificate is shared with the cache and other callers. Callers
// must not modify it or any nested data, including Leaf, certificate bytes, and
// the private key.
func (a *Authority) Certificate(host string) (*tls.Certificate, error) {
	parsed, err := hostport.Parse(host, "443")
	if err != nil || parsed.Port() != "443" {
		return nil, errors.New("invalid certificate host")
	}
	host = parsed.Host()
	// Deliberately keep signing under the cache lock: one lock bounds issuance
	// and avoids duplicate work. Misses block unrelated hits; defer separate
	// issuance locking until measurements show meaningful contention.
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if now.Before(a.cert.NotBefore) || !now.Before(a.cert.NotAfter) {
		return nil, errors.New("CA certificate is not currently valid")
	}
	if elem := a.leaves[host]; elem != nil {
		entry := elem.Value.(*leafEntry)
		cert := entry.cert.Leaf
		// Renewing a valid leaf already capped at CA expiry cannot extend its life.
		if cert != nil && !now.Before(cert.NotBefore) && now.Before(cert.NotAfter) &&
			(cert.NotAfter.Sub(now) > leafRenewal || cert.NotAfter.Equal(a.cert.NotAfter)) {
			a.order.MoveToBack(elem)
			return entry.cert, nil
		}
		leaf, err := a.makeLeaf(host, now)
		if err != nil {
			return nil, err
		}
		entry.cert = leaf
		a.order.MoveToBack(elem)
		return leaf, nil
	}
	leaf, err := a.makeLeaf(host, now)
	if err != nil {
		return nil, err
	}
	if a.order.Len() >= a.maxLeaves {
		oldest := a.order.Front()
		delete(a.leaves, oldest.Value.(*leafEntry).host)
		a.order.Remove(oldest)
	}
	a.leaves[host] = a.order.PushBack(&leafEntry{host: host, cert: leaf})
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
	t := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		t.IPAddresses = []net.IP{ip}
	} else {
		t.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	// The certificate was created with this key's public half, so no PEM
	// round trip or external key-pair validation is needed.
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
