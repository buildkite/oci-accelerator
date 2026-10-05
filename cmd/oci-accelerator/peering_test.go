package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peer"
)

func TestStartPeeringDisabledHasNoSideEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "peer")
	called := false
	done, cleanup, err := startPeering(context.Background(), config.Config{Peer: config.Peer{StateDir: dir}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("unexpected listen")
	})
	if err != nil || done != nil || cleanup != nil || called {
		t.Fatalf("startPeering disabled: done nil %t, cleanup nil %t, error %v, listen called %t", done == nil, cleanup == nil, err, called)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("disabled peering created state: %v", err)
	}
}

func TestPeerHTTPSExchangeAuthenticationPersistenceAndCleanup(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	listener := newTestListener(t)
	state := filepath.Join(root, "peer")
	cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: "self.test:7443", StateDir: state}, ShutdownTimeout: 100 * time.Millisecond}
	done, cleanup, err := startPeering(context.Background(), cfg, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	if done == nil || cleanup == nil {
		t.Fatal("enabled peering returned nil lifecycle values")
	}
	t.Cleanup(func() { _ = cleanup() })
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	want := peer.Message{Address: "self.test:7443", Peers: []string{newTestListener(t).Addr().String()}}
	client := peer.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	got, err := peer.Exchange(context.Background(), client, listener.Addr().String(), peer.Message{Address: want.Peers[0]})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated exchange = %#v, %v, want %#v", got, err, want)
	}

	post := func(client *http.Client, address string) error {
		defer client.CloseIdleConnections()
		req, err := http.NewRequest(http.MethodPost, "https://"+listener.Addr().String()+"/v1/peers/exchange", strings.NewReader(fmt.Sprintf(`{"address":%q,"peers":[]}`, address)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			return fmt.Errorf("request unexpectedly reached HTTP with status %d", resp.StatusCode)
		}
		return nil
	}
	unauthTLS := clientTLS.Clone()
	unauthTLS.GetClientCertificate = nil
	if err := post(peer.NewClient(unauthTLS), "unauth.test:7443"); err != nil {
		t.Fatal(err)
	}
	foreign := openPeerTestCA(t, filepath.Join(root, "foreign-ca"))
	_, foreignTLS, err := foreign.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS.RootCAs = clientTLS.RootCAs
	if err := post(peer.NewClient(foreignTLS), "foreign.test:7443"); err != nil {
		t.Fatal(err)
	}

	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("serve completion channel remains open after cleanup")
	}
	assertListenerClosed(t, listener)
	book, err := peer.OpenBook(state, cfg.Peer.Advertise)
	if err != nil {
		t.Fatalf("state lock not released: %v", err)
	}
	defer book.Close()
	if got := book.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened book = %#v, want %#v", got, want)
	}
}

func TestStartPeeringFailuresReleaseState(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	base := config.Config{Peer: config.Peer{Listen: "127.0.0.1:1", Advertise: "self.test:1", StateDir: filepath.Join(root, "state")}, ShutdownTimeout: time.Second}
	_, _, err := startPeering(context.Background(), base, signer, slog.Default(), func(string, string) (net.Listener, error) { return nil, errors.New("private bind") })
	if err == nil || err.Error() != "cannot listen on configured peer address" {
		t.Fatalf("bind error = %v", err)
	}
	book, err := peer.OpenBook(base.Peer.StateDir, base.Peer.Advertise)
	if err != nil {
		t.Fatalf("bind failure retained state lock: %v", err)
	}
	book.Close()

	bad := base
	bad.Peer.StateDir = filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(bad.Peer.StateDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := startPeering(context.Background(), bad, signer, slog.Default(), net.Listen); err == nil || err.Error() != "peer state startup failed" {
		t.Fatalf("state error = %v", err)
	}
}

func TestPeerServeFailureAndStalledHandshakeCleanup(t *testing.T) {
	t.Run("serve failure", func(t *testing.T) {
		root := t.TempDir()
		signer := openPeerTestCA(t, filepath.Join(root, "ca"))
		socket := newTestListener(t)
		failure := make(chan struct{})
		listener := &faultListener{Listener: socket, accepting: make(chan struct{}), fail: failure, acceptErr: errors.New("private address")}
		cfg := config.Config{Peer: config.Peer{Listen: socket.Addr().String(), Advertise: "self.test:1", StateDir: filepath.Join(root, "state")}, ShutdownTimeout: time.Second}
		done, cleanup, err := startPeering(context.Background(), cfg, signer, slog.Default(), listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		close(failure)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serve failure did not stop peer service")
		}
		if err := cleanup(); err == nil || err.Error() != "peer listener stopped unexpectedly" {
			t.Fatalf("cleanup error = %v", err)
		}
		book, err := peer.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
		if err != nil {
			t.Fatalf("serve failure retained state lock: %v", err)
		}
		book.Close()
	})

	t.Run("incomplete TLS handshake", func(t *testing.T) {
		root := t.TempDir()
		signer := openPeerTestCA(t, filepath.Join(root, "ca"))
		listener := newTestListener(t)
		cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: "self.test:1", StateDir: filepath.Join(root, "state")}, ShutdownTimeout: 10 * time.Millisecond}
		_, cleanup, err := startPeering(context.Background(), cfg, signer, slog.Default(), listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0x16}); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err == nil || !strings.Contains(err.Error(), "peer server cleanup failed") {
			t.Fatalf("cleanup error = %v, want deadline failure", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("stalled TLS connection remained open")
		}
		book, err := peer.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
		if err != nil {
			t.Fatalf("cleanup retained state lock: %v", err)
		}
		book.Close()
	})
}

func TestRunPeerCheckAndBindFailure(t *testing.T) {
	root := t.TempDir()
	proxyListener := newTestListener(t)
	peerAddress := newTestListener(t).Addr().String()
	state := filepath.Join(root, "peer-state")
	path := filepath.Join(root, "proxy.env")
	contents := fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_PEER_LISTEN=%s\nOCI_ACCELERATOR_PEER_ADVERTISE=self.test:7443\nOCI_ACCELERATOR_PEER_SEEDS=unreachable.test:7443\nOCI_ACCELERATOR_PEER_STATE_DIR=%s\n", proxyListener.Addr(), filepath.Join(root, "ca"), filepath.Join(root, "cache"), peerAddress, state)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := run(context.Background(), []string{"--env-file", path, "--check"}, io.Discard, io.Discard, func(string, string) (net.Listener, error) { called = true; return nil, errors.New("listen") }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("--check attempted network access")
	}
	for _, dir := range []string{filepath.Join(root, "ca"), filepath.Join(root, "cache"), state} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("--check created %s: %v", dir, err)
		}
	}
	var logs bytes.Buffer
	err := run(context.Background(), []string{"--env-file", path}, io.Discard, &logs, listenForTest(proxyListener))
	if err == nil || err.Error() != "cannot listen on configured peer address" {
		t.Fatalf("run bind error = %v", err)
	}
	if strings.Contains(logs.String(), "proxy ready") {
		t.Fatal("run announced readiness before peer bind")
	}
	assertListenerClosed(t, proxyListener)
}

func TestRunPeerOutageDoesNotAffectReadinessAndListenerFailureStopsRun(t *testing.T) {
	root := t.TempDir()
	proxyListener, metricsListener, peerListener := newTestListener(t), newTestListener(t), newTestListener(t)
	// Leave this local socket listening without accepting, so peer TLS cannot
	// complete. No external DNS or network fixture is involved.
	unresponsive := newTestListener(t)
	state := filepath.Join(root, "peers")
	path := filepath.Join(root, "proxy.env")
	contents := fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_METRICS_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_PEER_LISTEN=%s\nOCI_ACCELERATOR_PEER_ADVERTISE=%s\nOCI_ACCELERATOR_PEER_SEEDS=%s\nOCI_ACCELERATOR_PEER_STATE_DIR=%s\nOCI_ACCELERATOR_SHUTDOWN_TIMEOUT=100ms\n", proxyListener.Addr(), metricsListener.Addr(), filepath.Join(root, "ca"), filepath.Join(root, "cache"), peerListener.Addr(), peerListener.Addr(), unresponsive.Addr(), state)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = run(ctx, []string{"--env-file", path}, io.Discard, io.Discard, listenForTest(proxyListener, metricsListener, peerListener))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get("http://" + metricsListener.Addr().String() + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy did not become ready while seed was unresponsive")
		}
		time.Sleep(time.Millisecond)
	}
	for _, listener := range []*net.TCPListener{proxyListener, metricsListener} {
		response, err := client.Post("http://"+listener.Addr().String()+"/v1/peers/exchange", "application/json", strings.NewReader(`{"address":"bad.test:1"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("peer exchange available outside peer listener")
		}
	}
	if err := peerListener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("peer listener failure did not stop run")
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "peer listener stopped unexpectedly") {
		t.Fatalf("run error = %v", runErr)
	}
	assertListenerClosed(t, proxyListener)
	assertListenerClosed(t, metricsListener)
	book, err := peer.OpenBook(state, peerListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if got := book.Snapshot().Peers; !reflect.DeepEqual(got, []string{unresponsive.Addr().String()}) {
		t.Fatalf("persisted peers after outage = %v", got)
	}
}

func openPeerTestCA(t *testing.T, dir string) *ca.Authority {
	t.Helper()
	signer, err := ca.Open(dir, 16)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
