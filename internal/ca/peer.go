//go:build unix

package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/url"
	"sync"
	"time"
)

const (
	peerDNSName = "oci-accelerator-peer"
	peerURIName = "urn:oci-accelerator:peer:v1"
)

var peerURI = &url.URL{Scheme: "urn", Opaque: "oci-accelerator:peer:v1"}

// PeerTLSConfigs returns mutually authenticated TLS configurations for the
// fleet peer port. The client name identifies fleet membership, not a network
// endpoint. Both configurations share a renewable ephemeral peer certificate.
func (a *Authority) PeerTLSConfigs() (server, client *tls.Config, err error) {
	return a.peerTLSConfigs(time.Now)
}

func (a *Authority) peerTLSConfigs(now func() time.Time) (server, client *tls.Config, err error) {
	source := &peerCertificateSource{authority: a, now: now}
	if _, err := source.certificate(); err != nil {
		return nil, nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.cert)
	verifyPeer := func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("peer certificate is required")
		}
		return validatePeerProfile(state.PeerCertificates[0])
	}
	server = &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  roots,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return source.certificate()
		},
		VerifyConnection: verifyPeer,
	}
	client = &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		RootCAs:    roots,
		ServerName: peerDNSName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return source.certificate()
		},
		VerifyConnection: verifyPeer,
	}
	return server, client, nil
}

type peerCertificateSource struct {
	mu        sync.Mutex
	authority *Authority
	now       func() time.Time
	cert      *tls.Certificate
}

func (s *peerCertificateSource) certificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Before(s.authority.cert.NotBefore) || !now.Before(s.authority.cert.NotAfter) {
		return nil, errors.New("CA certificate is not currently valid")
	}
	if s.cert != nil && s.authority.leafUsable(s.cert.Leaf, now) {
		return s.cert, nil
	}
	cert, err := s.authority.makePeerCertificate(now)
	if err != nil {
		return nil, err
	}
	s.cert = cert
	return cert, nil
}

func (a *Authority) makePeerCertificate(now time.Time) (*tls.Certificate, error) {
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
		return nil, errors.New("CA expires too soon to issue a peer certificate")
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: peerDNSName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{peerDNSName},
		URIs:         []*url.URL{peerURI},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func validatePeerProfile(cert *x509.Certificate) error {
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != peerDNSName || len(cert.URIs) != 1 || cert.URIs[0].String() != peerURIName {
		return errors.New("certificate does not have the peer identity")
	}
	if len(cert.ExtKeyUsage) != 2 || !hasExtKeyUsage(cert, x509.ExtKeyUsageServerAuth) || !hasExtKeyUsage(cert, x509.ExtKeyUsageClientAuth) {
		return errors.New("certificate does not have the peer usages")
	}
	return nil
}

func hasExtKeyUsage(cert *x509.Certificate, usage x509.ExtKeyUsage) bool {
	for _, candidate := range cert.ExtKeyUsage {
		if candidate == usage {
			return true
		}
	}
	return false
}
