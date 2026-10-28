package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
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
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

func TestStartPeeringDisabledHasNoSideEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "peer")
	called := false
	service, err := startPeering(context.Background(), config.Config{Peer: config.Peer{StateDir: dir}}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("unexpected listen")
	})
	if err != nil || service != nil || called {
		t.Fatalf("startPeering disabled: service %v, error %v, listen called %t", service, err, called)
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
	cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: "self.test:5130", StateDir: state, HintRefreshTimeout: time.Minute, HintRefreshInterval: time.Minute, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: 100 * time.Millisecond}
	service, err := startPeering(context.Background(), cfg, signer, newPeerTestStore(t), slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || service.done == nil || service.close == nil || service.fetch == nil {
		t.Fatal("enabled peering returned nil lifecycle values")
	}
	done, cleanup := service.done, service.close
	t.Cleanup(func() { _ = cleanup() })
	serverTLS, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	selfID, err := os.ReadFile(filepath.Join(state, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewUnstartedServer(nil)
	peerAddress, peerID := peer.Listener.Addr().String(), "000000000001"
	peerSnapshot := func() peering.Message {
		return peering.Message{Address: peerAddress, NodeID: peerID, Peers: map[string]string{}}
	}
	peer.Config.Handler = peering.NewHandler(peerSnapshot, func(peering.Message) (peering.Message, error) {
		return peerSnapshot(), nil
	})
	peer.TLS = serverTLS
	peer.StartTLS()
	t.Cleanup(peer.Close)
	want := peering.Message{Address: "self.test:5130", NodeID: string(selfID), Peers: map[string]string{}}
	client := peering.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	got, err := peering.Exchange(context.Background(), client, listener.Addr().String(), peering.Message{Address: peerAddress, NodeID: peerID, Peers: map[string]string{}})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated exchange = %#v, %v, want %#v", got, err, want)
	}
	// Only the successful reverse probe admits the inbound sender. Its
	// verified membership must then survive shutdown and reopening.
	want.Peers[peerAddress] = peerID
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err = peering.Exchange(context.Background(), client, listener.Addr().String(), peering.Message{Address: peerAddress, NodeID: peerID, Peers: map[string]string{}})
		if err == nil && reflect.DeepEqual(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified membership = %#v, %v, want %#v", got, err, want)
		}
		time.Sleep(time.Millisecond)
	}
	response, err := client.Get("https://" + listener.Addr().String() + "/v1/peers")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot peering.Message
	decodeErr := json.NewDecoder(response.Body).Decode(&snapshot)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("authenticated snapshot = %d, %#v, %v", response.StatusCode, snapshot, decodeErr)
	}

	post := func(client *http.Client, address string) error {
		defer client.CloseIdleConnections()
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			req, err := http.NewRequest(method, "https://"+listener.Addr().String()+"/v1/peers", strings.NewReader(fmt.Sprintf(`{"address":%q,"nodeId":"000000000002","peers":{}}`, address)))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return fmt.Errorf("request unexpectedly reached HTTP with status %d", resp.StatusCode)
			}
		}
		return nil
	}
	unauthTLS := clientTLS.Clone()
	unauthTLS.GetClientCertificate = nil
	if err := post(peering.NewClient(unauthTLS), "unauth.test:5130"); err != nil {
		t.Fatal(err)
	}
	foreign := openPeerTestCA(t, filepath.Join(root, "foreign-ca"))
	_, foreignTLS, err := foreign.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS.RootCAs = clientTLS.RootCAs
	if err := post(peering.NewClient(foreignTLS), "foreign.test:5130"); err != nil {
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
	book, err := peering.OpenBook(context.Background(), state, cfg.Peer.Advertise, nil, nil)
	if err != nil {
		t.Fatalf("state lock not released: %v", err)
	}
	defer book.Close()
	if got := book.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened book = %#v, want %#v", got, want)
	}
}

func TestPeerServesCacheKeysAndEntriesOnlyToAuthenticatedPeers(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	listener := newTestListener(t)
	state := filepath.Join(root, "peer")
	store := newPeerTestStore(t)
	first := commitPeerTestEntry(t, store, "first inventory entry")
	second := commitPeerTestEntry(t, store, "second inventory entry")
	keys := []string{first.Key, second.Key}
	cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: "self.test:5130", StateDir: state, HintRefreshTimeout: time.Minute, HintRefreshInterval: time.Minute, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: 100 * time.Millisecond}
	cfg.Cache.FillTimeout = time.Second
	service, err := startPeering(context.Background(), cfg, signer, store, slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.close() })
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	selfID, err := os.ReadFile(filepath.Join(state, "node-id"))
	if err != nil {
		t.Fatal(err)
	}

	get := func(client *http.Client) (*http.Response, error) {
		defer client.CloseIdleConnections()
		return client.Get("https://" + listener.Addr().String() + "/v1/cache/keys")
	}
	response, err := get(peering.NewClient(clientTLS))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"nodeId":%q,"keys":[%q,%q],"next":""}`, strings.TrimSpace(string(selfID)), keys[0], keys[1])
	if response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("authenticated keys = %d %s, want 200 %s", response.StatusCode, body, want)
	}

	client := peering.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	entry, err := peering.FetchEntry(context.Background(), client, listener.Addr().String(), string(selfID), first)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(entry)
	closeErr := entry.Close()
	if readErr != nil || closeErr != nil || string(got) != "first inventory entry" {
		t.Fatalf("authenticated entry = %q, read %v, close %v", got, readErr, closeErr)
	}

	unauthTLS := clientTLS.Clone()
	unauthTLS.GetClientCertificate = nil
	if response, err := get(peering.NewClient(unauthTLS)); err == nil {
		response.Body.Close()
		t.Fatalf("unauthenticated keys request reached HTTP with status %d", response.StatusCode)
	}
	foreign := openPeerTestCA(t, filepath.Join(root, "foreign-ca"))
	_, foreignTLS, err := foreign.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS.RootCAs = clientTLS.RootCAs
	for name, outsiderTLS := range map[string]*tls.Config{"no certificate": unauthTLS, "foreign certificate": foreignTLS} {
		outsider := peering.NewClient(outsiderTLS)
		defer outsider.CloseIdleConnections()
		if entry, err := peering.FetchEntry(context.Background(), outsider, listener.Addr().String(), string(selfID), first); entry != nil || err == nil || errors.Is(err, peering.ErrEntryMiss) {
			if entry != nil {
				entry.Close()
			}
			t.Fatalf("%s entry request = %v, want authentication failure", name, err)
		}
	}
}

func TestPeersTransferEntriesOnDiscoveryPeriodicRefreshAndRestart(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	storeA, storeB := newPeerTestStore(t), newPeerTestStore(t)
	metaA := commitPeerTestEntry(t, storeA, "entry from A")
	metaB := commitPeerTestEntry(t, storeB, "entry from B")
	start := func(name string, store *cache.Store, seeds []string, interval time.Duration) (string, *peerService) {
		t.Helper()
		listener := newTestListener(t)
		cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: listener.Addr().String(), Seeds: seeds, StateDir: filepath.Join(root, name), HintRefreshTimeout: time.Minute, HintRefreshInterval: interval, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: time.Second}
		cfg.Cache.FillTimeout = 10 * time.Second
		service, err := startPeering(context.Background(), cfg, signer, store, logger, listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.close(); err != nil {
				t.Error(err)
			}
		})
		return listener.Addr().String(), service
	}

	addressB, serviceB := start("b", storeB, nil, 100*time.Millisecond)
	_, serviceA := start("a", storeA, []string{addressB}, 100*time.Millisecond)
	// A verifies B through its own exchange. B probes A back before
	// admitting it. Each must learn a live hint and serve the other's bytes.
	waitForPeerEntry(t, serviceA, metaB, "entry from B")
	waitForPeerEntry(t, serviceB, metaA, "entry from A")
	if reader, err := storeA.Get(context.Background(), metaB); !errors.Is(err, cache.ErrMiss) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("fetch unexpectedly populated the requesting store: %v", err)
	}
	secondA := commitPeerTestEntry(t, storeA, "later entry from A")
	secondB := commitPeerTestEntry(t, storeB, "later entry from B")
	waitForPeerEntry(t, serviceA, secondB, "later entry from B")
	waitForPeerEntry(t, serviceB, secondA, "later entry from A")
	if got := storeB.Stats(); got.FillSuccess != 2 || got.Fills != 0 || got.Readers != 0 {
		t.Fatalf("serving entries filled the source or leaked readers: %+v", got)
	}

	// After a restart A has no hints, so it fetches from B, which its book
	// already knows, without any new discovery.
	if err := serviceA.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serviceA.done:
	default:
		t.Fatal("peer service did not finish shutdown")
	}
	// An hourly interval cannot satisfy this bounded startup assertion.
	_, restartedA := start("a", storeA, nil, time.Hour)
	waitForPeerEntry(t, restartedA, metaB, "entry from B")

	// Evict both entries from the two-slot source. A's hint is now stale.
	commitPeerTestEntry(t, storeB, "replacement one")
	commitPeerTestEntry(t, storeB, "replacement two")
	if got := storeB.Stats().Evictions; got != 2 {
		t.Fatalf("source evictions = %d, want 2", got)
	}
	if body, err := restartedA.fetch(context.Background(), metaB); body != nil || !errors.Is(err, peering.ErrEntryMiss) {
		if body != nil {
			body.Close()
		}
		t.Fatalf("stale hinted entry = %v, want definitive peer miss", err)
	}
	// Restoring the bytes cannot make the forgotten hint usable. No periodic
	// refresh runs in this scenario, so another transfer would be a regression.
	commitPeerTestEntry(t, storeB, "entry from B")
	if body, err := restartedA.fetch(context.Background(), metaB); body != nil || err == nil || err.Error() != "unknown peer" {
		if body != nil {
			body.Close()
		}
		t.Fatalf("forgotten provider was contacted after restoring its entry: %v", err)
	}
}

func TestStartPeeringFailuresReleaseState(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	base := config.Config{Peer: config.Peer{Listen: "127.0.0.1:1", Advertise: "self.test:1", StateDir: filepath.Join(root, "state")}, ShutdownTimeout: time.Second}
	store := newPeerTestStore(t)
	_, err := startPeering(context.Background(), base, signer, store, slog.Default(), func(string, string) (net.Listener, error) { return nil, errors.New("private bind") })
	if err == nil || err.Error() != "cannot listen on configured peer address" {
		t.Fatalf("bind error = %v", err)
	}
	book, err := peering.OpenBook(context.Background(), base.Peer.StateDir, base.Peer.Advertise, nil, nil)
	if err != nil {
		t.Fatalf("bind failure retained state lock: %v", err)
	}
	book.Close()

	bad := base
	bad.Peer.StateDir = filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(bad.Peer.StateDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startPeering(context.Background(), bad, signer, store, slog.Default(), net.Listen); err == nil || err.Error() != "peer state startup failed: peer address book error" {
		t.Fatalf("state error = %v", err)
	}
}

func TestRunRejectsClonedPeerIdentityBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	seed := newTestListener(t)
	seedState := filepath.Join(root, "seed")
	seedCfg := config.Config{Peer: config.Peer{Listen: seed.Addr().String(), Advertise: seed.Addr().String(), StateDir: seedState, HintRefreshTimeout: time.Minute, HintRefreshInterval: time.Minute, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: time.Second}
	service, err := startPeering(context.Background(), seedCfg, signer, newPeerTestStore(t), slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(seed))
	if err != nil {
		t.Fatal(err)
	}
	defer service.close()
	id, err := os.ReadFile(filepath.Join(seedState, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	cloneState := filepath.Join(root, "clone")
	if err := os.Mkdir(cloneState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneState, "node-id"), id, 0o600); err != nil {
		t.Fatal(err)
	}
	proxyListener, metricsListener, peerListener := newTestListener(t), newTestListener(t), newTestListener(t)
	path := filepath.Join(root, "proxy.env")
	contents := fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_METRICS_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_PEER_LISTEN=%s\nOCI_ACCELERATOR_PEER_ADVERTISE=%s\nOCI_ACCELERATOR_PEER_SEEDS=%s\nOCI_ACCELERATOR_PEER_STATE_DIR=%s\n", proxyListener.Addr(), metricsListener.Addr(), filepath.Join(root, "ca"), filepath.Join(root, "cache"), peerListener.Addr(), peerListener.Addr(), seed.Addr(), cloneState)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	err = run(context.Background(), []string{"--env-file", path}, io.Discard, &logs, listenForTest(proxyListener, metricsListener, peerListener))
	if err == nil || !strings.Contains(err.Error(), "persisted peer node ID "+string(id)+" conflicts with "+seed.Addr().String()) {
		t.Fatalf("cloned identity startup = %v", err)
	}
	if strings.Contains(logs.String(), "proxy ready") {
		t.Fatal("announced readiness with a confirmed clone")
	}
	assertListenerClosed(t, proxyListener)
	assertListenerClosed(t, metricsListener)
	book, err := peering.OpenBook(context.Background(), cloneState, peerListener.Addr().String(), nil, nil)
	if err != nil {
		t.Fatalf("startup failure retained state lock: %v", err)
	}
	defer book.Close()
	if book.Snapshot().NodeID != string(id) {
		t.Fatal("startup failure rotated persisted identity")
	}
}

func TestPeerServeFailureAndStalledHandshakeCleanup(t *testing.T) {
	t.Run("serve failure", func(t *testing.T) {
		root := t.TempDir()
		signer := openPeerTestCA(t, filepath.Join(root, "ca"))
		socket := newTestListener(t)
		failure := make(chan struct{})
		listener := &faultListener{Listener: socket, accepting: make(chan struct{}), fail: failure, acceptErr: errors.New("private address")}
		cfg := config.Config{Peer: config.Peer{Listen: socket.Addr().String(), Advertise: "self.test:1", StateDir: filepath.Join(root, "state"), HintRefreshTimeout: time.Minute, HintRefreshInterval: time.Minute, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: time.Second}
		service, err := startPeering(context.Background(), cfg, signer, newPeerTestStore(t), slog.Default(), listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.close() })
		done, cleanup := service.done, service.close
		close(failure)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serve failure did not stop peer service")
		}
		if err := cleanup(); err == nil || err.Error() != "peer listener stopped unexpectedly" {
			t.Fatalf("cleanup error = %v", err)
		}
		book, err := peering.OpenBook(context.Background(), cfg.Peer.StateDir, cfg.Peer.Advertise, nil, nil)
		if err != nil {
			t.Fatalf("serve failure retained state lock: %v", err)
		}
		book.Close()
	})

	t.Run("incomplete TLS handshake", func(t *testing.T) {
		root := t.TempDir()
		signer := openPeerTestCA(t, filepath.Join(root, "ca"))
		listener := &nextAcceptListener{Listener: newTestListener(t), nextAccept: make(chan struct{})}
		cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: "self.test:1", StateDir: filepath.Join(root, "state"), HintRefreshTimeout: time.Minute, HintRefreshInterval: time.Minute, HintFullRefreshInterval: time.Hour}, ShutdownTimeout: 10 * time.Millisecond}
		service, err := startPeering(context.Background(), cfg, signer, newPeerTestStore(t), slog.Default(), listenForTest(listener))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.close() })
		cleanup := service.close
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0x16}); err != nil {
			t.Fatal(err)
		}

		// net/http registers StateNew before accepting another connection.
		// A client write alone does not prove that the server owns the socket.
		select {
		case <-listener.nextAccept:
		case <-time.After(time.Second):
			t.Fatal("server did not register the stalled TLS connection")
		}

		if err := cleanup(); err == nil || !strings.Contains(err.Error(), "peer server cleanup failed") {
			t.Fatalf("cleanup error = %v, want deadline failure", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("stalled TLS connection remained open")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("stalled TLS connection timed out instead of closing: %v", err)
		}
		book, err := peering.OpenBook(context.Background(), cfg.Peer.StateDir, cfg.Peer.Advertise, nil, nil)
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
	contents := fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=example.com\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_PEER_LISTEN=%s\nOCI_ACCELERATOR_PEER_ADVERTISE=self.test:5130\nOCI_ACCELERATOR_PEER_SEEDS=unreachable.test:5130\nOCI_ACCELERATOR_PEER_STATE_DIR=%s\n", proxyListener.Addr(), filepath.Join(root, "ca"), filepath.Join(root, "cache"), peerAddress, state)
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
	for _, identified := range []bool{false, true} {
		t.Run(fmt.Sprintf("identified=%t", identified), func(t *testing.T) {
			root := t.TempDir()
			proxyListener, metricsListener, peerListener := newTestListener(t), newTestListener(t), newTestListener(t)
			// Leave this local socket listening without accepting, so peer TLS cannot
			// complete. No external DNS or network fixture is involved.
			unresponsive := newTestListener(t)
			state := filepath.Join(root, "peers")
			// Persist an identified peer, not just a seed, so startup queues an
			// inventory refresh as well as discovery. Only the bounded identity
			// bootstrap may delay readiness, not the background workers.
			if identified {
				signer := openPeerTestCA(t, filepath.Join(root, "ca"))
				serverTLS, clientTLS, err := signer.PeerTLSConfigs()
				if err != nil {
					t.Fatal(err)
				}
				address := unresponsive.Addr().(*net.TCPAddr)
				snapshot := func() peering.Message {
					return peering.Message{Address: address.String(), NodeID: "000000000002", Peers: map[string]string{}}
				}
				fixture := httptest.NewUnstartedServer(peering.NewHandler(snapshot, func(peering.Message) (peering.Message, error) {
					return snapshot(), nil
				}))
				fixture.Listener.Close()
				fixture.Listener = unresponsive
				fixture.TLS = serverTLS
				fixture.StartTLS()
				t.Cleanup(fixture.Close)
				book, err := peering.OpenBook(context.Background(), state, peerListener.Addr().String(), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { book.Close() })
				if _, err := book.AddSeeds([]string{address.String()}); err != nil {
					t.Fatal(err)
				}
				discovery := peering.NewDiscovery(book, peering.NewClient(clientTLS), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); discovery.Run(ctx) }()
				stop := func() { cancel(); <-done }
				t.Cleanup(stop)
				deadline := time.Now().Add(3 * time.Second)
				for book.Snapshot().Peers[address.String()] != "000000000002" {
					if time.Now().After(deadline) {
						t.Fatal("fixture peer was not verified")
					}
					time.Sleep(time.Millisecond)
				}
				stop()
				if err := book.Close(); err != nil {
					t.Fatal(err)
				}

				// Duplicate the listening socket before stopping TLS, retaining
				// the verified address without accepting new connections.
				file, err := unresponsive.File()
				if err != nil {
					t.Fatal(err)
				}
				retained, err := net.FileListener(file)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { retained.Close() })
				fixture.Close()
				unresponsive = retained.(*net.TCPListener)
			}
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
			deadline := time.Now().Add(7 * time.Second)
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
				for _, path := range []string{"/v1/peers", "/v1/cache/entry"} {
					response, err := client.Post("http://"+listener.Addr().String()+path, "application/json", strings.NewReader(`{"address":"bad.test:1","nodeId":"000000000001","peers":{}}`))
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode == http.StatusOK || response.Header.Get("OCI-Accelerator-Node-ID") != "" {
						t.Fatalf("peer endpoint %s available outside peer listener", path)
					}
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
			book, err := peering.OpenBook(context.Background(), state, peerListener.Addr().String(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			if !identified && len(book.Addresses()) != 0 {
				t.Fatal("reopening without configuration retained an unverified seed")
			}
			if _, err := book.AddSeeds([]string{unresponsive.Addr().String()}); err != nil {
				t.Fatal(err)
			}
			if got := book.Addresses(); !reflect.DeepEqual(got, []string{unresponsive.Addr().String()}) {
				t.Fatalf("persisted addresses after outage = %v", got)
			}
			if got := book.Snapshot().Peers; identified {
				if len(got) != 1 || got[unresponsive.Addr().String()] != "000000000002" {
					t.Fatalf("identified peer changed during outage: %v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("unresolved seed exported with an identity: %v", got)
			}
		})
	}
}

type nextAcceptListener struct {
	net.Listener
	nextAccept chan struct{}
	calls      int
}

func (l *nextAcceptListener) Accept() (net.Conn, error) {
	if l.calls == 1 {
		close(l.nextAccept)
	}
	l.calls++

	return l.Listener.Accept()
}

func newPeerTestStore(t *testing.T) *cache.Store {
	t.Helper()
	store, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), cache.Limits{MaxBytes: 1 << 20, MaxEntries: 2, MaxFills: 8, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func waitForPeerEntry(t *testing.T, service *peerService, meta cache.Metadata, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		body, err := service.fetch(ctx, meta)
		if err == nil {
			got, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil || closeErr != nil || string(got) != want {
				t.Fatalf("peer entry = %q, read %v, close %v, want %q", got, readErr, closeErr, want)
			}
			return
		}
		// Only wait for asynchronous discovery and inventory publication. Never
		// retry a dispatched transfer failure or a body mismatch.
		if err.Error() != "unknown peer" {
			t.Fatalf("peer transfer failed: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no provider learned for %s: %v", meta.Key, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func commitPeerTestEntry(t *testing.T, store *cache.Store, body string) cache.Metadata {
	t.Helper()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	meta := cache.Metadata{Key: digest, RequestKey: digest, Digest: "sha256:" + digest, Size: int64(len(body)), MediaType: "application/octet-stream"}
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer fill.Abort()
	if _, err := io.WriteString(fill, body); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	return meta
}

func openPeerTestCA(t *testing.T, dir string) *ca.Authority {
	t.Helper()
	signer, err := ca.Open(dir, 16)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
