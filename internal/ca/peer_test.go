//go:build unix

package ca

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPeerTLSHandshake(t *testing.T) {
	a := openPeerTestAuthority(t)
	server, client, err := a.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if server.MinVersion != tls.VersionTLS13 || client.MinVersion != tls.VersionTLS13 {
		t.Fatal("peer TLS does not require TLS 1.3")
	}
	if server.ClientAuth != tls.RequireAndVerifyClientCert || client.InsecureSkipVerify {
		t.Fatal("peer TLS does not require normal mutual certificate verification")
	}
	if len(server.NextProtos) != 1 || server.NextProtos[0] != "http/1.1" || len(client.NextProtos) != 1 || client.NextProtos[0] != "http/1.1" {
		t.Fatal("peer TLS does not restrict ALPN to HTTP/1.1")
	}
	if client.ServerName != peerDNSName {
		t.Fatalf("client ServerName = %q", client.ServerName)
	}
	peerHandshake(t, server, client, true)
}

func TestPeerTLSRejectsUntrustedAndMissingClients(t *testing.T) {
	a := openPeerTestAuthority(t)
	b := openPeerTestAuthority(t)
	serverA, _, err := a.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	_, clientB, err := b.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	peerHandshake(t, serverA, clientB, false)

	_, clientA, err := a.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	withoutCertificate := clientA.Clone()
	withoutCertificate.GetClientCertificate = nil
	peerHandshake(t, serverA, withoutCertificate, false)
}

func TestPeerTLSRejectsRegistryCertificateOnBothSides(t *testing.T) {
	a := openPeerTestAuthority(t)
	peerServer, peerClient, err := a.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := a.Certificate(peerDNSName)
	if err != nil {
		t.Fatal(err)
	}

	registryServer := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
		Certificates: []tls.Certificate{*registry},
	}
	peerHandshake(t, registryServer, peerClient, false)

	registryClient := peerClient.Clone()
	registryClient.GetClientCertificate = nil
	registryClient.Certificates = []tls.Certificate{*registry}
	peerHandshake(t, peerServer, registryClient, false)
}

func TestPeerCertificateRenewalAndCAExpiry(t *testing.T) {
	a := openPeerTestAuthority(t)
	now := a.cert.NotBefore.Add(2 * time.Hour)
	server, client, err := a.peerTLSConfigs(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := server.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientFirst, err := client.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != clientFirst {
		t.Fatal("server and client did not share the peer certificate source")
	}
	now = first.Leaf.NotAfter.Add(-leafRenewal)
	renewed, err := server.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first || !renewed.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Fatal("near-expiry peer certificate was not renewed")
	}

	now = a.cert.NotAfter.Add(-30 * time.Minute)
	cappedServer, _, err := a.peerTLSConfigs(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	capped, err := cappedServer.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !capped.Leaf.NotAfter.Equal(a.cert.NotAfter) {
		t.Fatal("peer certificate was not capped at CA expiry")
	}
	now = a.cert.NotAfter
	if _, err := cappedServer.GetCertificate(nil); err == nil {
		t.Fatal("certificate callback accepted an expired CA")
	}
	if _, _, err := a.peerTLSConfigs(func() time.Time { return now }); err == nil {
		t.Fatal("config construction accepted an expired CA")
	}
}

func openPeerTestAuthority(t *testing.T) *Authority {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), "ca"), 1)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func peerHandshake(t *testing.T, serverConfig, clientConfig *tls.Config, wantSuccess bool) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	server := tls.Server(serverConn, serverConfig)
	client := tls.Client(clientConn, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.HandshakeContext(ctx) }()
	clientErr := client.HandshakeContext(ctx)
	if clientErr != nil {
		cancel()
	}
	serverErr := <-serverResult
	if wantSuccess {
		if clientErr != nil || serverErr != nil {
			t.Fatalf("handshake: client=%v server=%v", clientErr, serverErr)
		}
		if client.ConnectionState().NegotiatedProtocol != "http/1.1" {
			t.Fatalf("negotiated protocol = %q", client.ConnectionState().NegotiatedProtocol)
		}
		return
	}
	if clientErr == nil && serverErr == nil {
		t.Fatal("handshake unexpectedly succeeded")
	}
}
