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
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
	"github.com/buildkite/oci-accelerator/internal/proxy"
)

func TestStartPeeringDisabledHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "peer")
	called := false
	store := openPeerTestStore(t, filepath.Join(root, "cache"))
	before := cacheDirEntries(t, filepath.Join(root, "cache"))
	done, availability, transfer, cleanup, err := startPeering(context.Background(), config.Config{Peer: config.Peer{StateDir: dir}}, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("unexpected listen")
	})
	if err != nil || done != nil || availability != nil || transfer != nil || cleanup != nil || called {
		t.Fatalf("startPeering disabled: done nil %t, availability nil %t, transfer nil %t, cleanup nil %t, error %v, listen called %t", done == nil, availability == nil, transfer == nil, cleanup == nil, err, called)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("disabled peering created state: %v", err)
	}
	if after := cacheDirEntries(t, filepath.Join(root, "cache")); !reflect.DeepEqual(after, before) {
		t.Fatalf("disabled peering changed the cache directory: %v then %v", before, after)
	}
}

func TestPeerHTTPSExchangeAuthenticationPersistenceAndCleanup(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	listener := newTestListener(t)
	state := filepath.Join(root, "peer")
	cfg := peerTestConfig(listener.Addr().String(), "self.test:7443", state, nil)
	cfg.ShutdownTimeout = 100 * time.Millisecond
	done, _, transfer, cleanup, err := startPeering(context.Background(), cfg, signer, openPeerTestStore(t, filepath.Join(root, "cache")), slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	if done == nil || transfer == nil || cleanup == nil {
		t.Fatal("enabled peering returned nil lifecycle values")
	}
	t.Cleanup(func() { _ = cleanup() })
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	selfID, err := os.ReadFile(filepath.Join(state, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	peerAddress, peerID := newTestListener(t).Addr().String(), "000000000001"
	want := peering.Message{Address: "self.test:7443", NodeID: string(selfID), Peers: map[string]string{peerAddress: peerID}}
	client := peering.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	got, err := peering.Exchange(context.Background(), client, listener.Addr().String(), peering.Message{Address: peerAddress, NodeID: peerID, Peers: map[string]string{}})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated exchange = %#v, %v, want %#v", got, err, want)
	}

	post := func(client *http.Client, address string) error {
		defer client.CloseIdleConnections()
		req, err := http.NewRequest(http.MethodPost, "https://"+listener.Addr().String()+"/v1/peers/exchange", strings.NewReader(fmt.Sprintf(`{"address":%q,"nodeId":"000000000002","peers":{}}`, address)))
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
	if err := post(peering.NewClient(unauthTLS), "unauth.test:7443"); err != nil {
		t.Fatal(err)
	}
	foreign := openPeerTestCA(t, filepath.Join(root, "foreign-ca"))
	_, foreignTLS, err := foreign.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS.RootCAs = clientTLS.RootCAs
	if err := post(peering.NewClient(foreignTLS), "foreign.test:7443"); err != nil {
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
	book, err := peering.OpenBook(state, cfg.Peer.Advertise)
	if err != nil {
		t.Fatalf("state lock not released: %v", err)
	}
	defer book.Close()
	if got := book.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened book = %#v, want %#v", got, want)
	}
}

func TestPeerAvailabilityPropagatesOverMutualTLS(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	listenerA, listenerB := newTestListener(t), newTestListener(t)
	storeA, storeB := openPeerTestStore(t, filepath.Join(root, "cache-a")), openPeerTestStore(t, filepath.Join(root, "cache-b"))
	start := func(name string, listener net.Listener, seeds []string, store *cache.Store) (*peering.Availability, func() error) {
		cfg := peerTestConfig(listener.Addr().String(), listener.Addr().String(), filepath.Join(root, name), seeds)
		cfg.ShutdownTimeout = 100 * time.Millisecond
		_, availability, _, cleanup, err := startPeering(context.Background(), cfg, signer, store, slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		stop := sync.OnceValue(cleanup)
		t.Cleanup(func() {
			if err := stop(); err != nil {
				t.Error(err)
			}
		})
		return availability, stop
	}
	b, stopB := start("b", listenerB, nil, storeB)
	a, _ := start("a", listenerA, []string{listenerB.Addr().String()}, storeA)
	idA, idB := readNodeID(t, filepath.Join(root, "a")), readNodeID(t, filepath.Join(root, "b"))
	// A pass that starts before discovery resolves B finishes without sending,
	// so commit only after both books hold the other's identity.
	waitForPeers(t, filepath.Join(root, "a"), map[string]string{listenerB.Addr().String(): idB})
	waitForPeers(t, filepath.Join(root, "b"), map[string]string{listenerA.Addr().String(): idA})
	key := commitPeerTestEntry(t, storeA, []byte("availability entry")).Key
	deadline := time.Now().Add(20 * time.Second)
	for !reflect.DeepEqual(b.Owners(key), []string{idA}) || a.Stats().Acknowledged == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("B owners = %v, A stats = %+v, want A acknowledged as owner", b.Owners(key), a.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The availability endpoint shares the fleet mTLS requirement.
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	unauthTLS := clientTLS.Clone()
	unauthTLS.GetClientCertificate = nil
	client := peering.NewClient(unauthTLS)
	defer client.CloseIdleConnections()
	body := `{"senderNodeId":"000000000009","recipientNodeId":"000000000008","claims":[]}`
	if response, err := client.Post("https://"+listenerB.Addr().String()+"/v1/cache/availability", "application/json", strings.NewReader(body)); err == nil {
		response.Body.Close()
		t.Fatalf("unauthenticated availability request reached HTTP with status %d", response.StatusCode)
	}

	var published atomic.Pointer[peering.Availability]
	published.Store(a)
	server := newMetricsServer(config.Config{}, proxy.New(config.Config{}, nil, nil, nil), &published)
	stats := a.Stats()
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if after := a.Stats(); after != stats {
		t.Fatalf("stats changed during the scrape: %+v then %+v", stats, after)
	}
	for _, line := range []string{
		"oci_accelerator_availability_hints 1\n",
		"oci_accelerator_availability_hint_evictions_total 0\n",
		fmt.Sprintf("oci_accelerator_availability_sends_total{outcome=%q} %d\n", "acknowledged", stats.Acknowledged),
		fmt.Sprintf("oci_accelerator_availability_sends_total{outcome=%q} %d\n", "failed", stats.Failed),
		"oci_accelerator_availability_checkpoint_failures_total 0\n",
	} {
		if !strings.Contains(response.Body.String(), line) {
			t.Fatalf("metrics missing %q", line)
		}
	}

	// Cleanup checkpoints B's hints, and a restart restores them without any
	// new delivery.
	if err := stopB(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "b", "availability.json")); err != nil {
		t.Fatalf("no availability checkpoint after cleanup: %v", err)
	}
	restartedListener := newTestListener(t)
	restarted, _ := start("b", restartedListener, nil, storeB)
	if got := restarted.Owners(key); !reflect.DeepEqual(got, []string{idA}) {
		t.Fatalf("B owners after restart = %v", got)
	}
}

func TestStartPeeringFailuresReleaseState(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	base := peerTestConfig("127.0.0.1:1", "self.test:1", filepath.Join(root, "state"), nil)
	_, _, _, _, err := startPeering(context.Background(), base, signer, nil, slog.Default(), func(string, string) (net.Listener, error) { return nil, errors.New("private bind") })
	if err == nil || err.Error() != "cannot listen on configured peer address" {
		t.Fatalf("bind error = %v", err)
	}
	book, err := peering.OpenBook(base.Peer.StateDir, base.Peer.Advertise)
	if err != nil {
		t.Fatalf("bind failure retained state lock: %v", err)
	}
	book.Close()

	bad := base
	bad.Peer.StateDir = filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(bad.Peer.StateDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := startPeering(context.Background(), bad, signer, nil, slog.Default(), net.Listen); err == nil || err.Error() != "peer state startup failed" {
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
		cfg := peerTestConfig(socket.Addr().String(), "self.test:1", filepath.Join(root, "state"), nil)
		done, _, _, cleanup, err := startPeering(context.Background(), cfg, signer, openPeerTestStore(t, filepath.Join(root, "cache")), slog.Default(), listenForTest(listener))
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
		book, err := peering.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
		if err != nil {
			t.Fatalf("serve failure retained state lock: %v", err)
		}
		book.Close()
	})

	t.Run("incomplete TLS handshake", func(t *testing.T) {
		root := t.TempDir()
		signer := openPeerTestCA(t, filepath.Join(root, "ca"))
		listener := newTestListener(t)
		cfg := peerTestConfig(listener.Addr().String(), "self.test:1", filepath.Join(root, "state"), nil)
		cfg.ShutdownTimeout = 10 * time.Millisecond
		_, _, _, cleanup, err := startPeering(context.Background(), cfg, signer, openPeerTestStore(t, filepath.Join(root, "cache")), slog.Default(), listenForTest(listener))
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
		book, err := peering.OpenBook(cfg.Peer.StateDir, cfg.Peer.Advertise)
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
		response, err := client.Post("http://"+listener.Addr().String()+"/v1/peers/exchange", "application/json", strings.NewReader(`{"address":"bad.test:1","nodeId":"000000000001","peers":{}}`))
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
	book, err := peering.OpenBook(state, peerListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if got := book.Addresses(); !reflect.DeepEqual(got, []string{unresponsive.Addr().String()}) {
		t.Fatalf("persisted addresses after outage = %v", got)
	}
	if got := book.Snapshot().Peers; len(got) != 0 {
		t.Fatalf("unresolved seed exported with an identity: %v", got)
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

func cacheDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
