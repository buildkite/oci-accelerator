package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

// peerFetchBodySize exceeds loopback socket buffers, so an unread stream
// keeps its connection and the serving handler busy.
const peerFetchBodySize = 32 << 20

func TestPeerFetchCommitsAndAdvertisesOverSharedListener(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	mesh := startPeerMesh(t, root, signer, 30*time.Second)
	a, b, c := mesh[0], mesh[1], mesh[2]

	body := peerTestBody(peerFetchBodySize)
	meta := commitPeerTestEntry(t, b.store, body)
	waitForOwners(t, a.availability, meta.Key, []string{b.id})

	stream, err := a.transfer.Fetch(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first := make([]byte, 64<<10)
	if _, err := io.ReadFull(stream, first); err != nil {
		t.Fatal(err)
	}

	// Discovery shares the listener and keeps working while B streams.
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	client := peering.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	exchanged, err := peering.Exchange(context.Background(), client, b.address, peering.Message{Address: c.address, NodeID: c.id, Peers: map[string]string{}})
	if err != nil || exchanged.NodeID != b.id {
		t.Fatalf("exchange during a transfer = %+v, %v", exchanged, err)
	}

	// The requester verifies the digest at Commit, as the proxy does.
	fill, err := a.store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fill, stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	assertCachedBytes(t, a.store, meta, body)

	// C learns A's new ownership from A's commit notification.
	waitForOwners(t, c.availability, meta.Key, sortedIDs(a.id, b.id))

	// The fetch endpoint shares the fleet mutual-TLS requirement.
	assertFetchRejectedAtTLS(t, root, signer, a.address, a.id, meta)
}

func TestPeerCleanupCancelsActiveTransfers(t *testing.T) {
	for _, side := range []string{"serving", "fetching"} {
		t.Run(side, func(t *testing.T) {
			root := t.TempDir()
			signer := openPeerTestCA(t, filepath.Join(root, "ca"))
			// Independent fleets keep the first failure's owner cooldown from
			// affecting the second shutdown case.
			mesh := startPeerMesh(t, root, signer, time.Hour)
			a, b := mesh[0], mesh[1]
			meta := commitPeerTestEntry(t, b.store, peerTestBody(peerFetchBodySize))
			waitForOwners(t, a.availability, meta.Key, []string{b.id})
			stream, err := a.transfer.Fetch(context.Background(), meta)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			stop := b.stop
			if side == "fetching" {
				stop = a.stop
			}
			start := time.Now()
			if err := stop(); err != nil {
				t.Fatalf("cleanup with an active stream = %v", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("cleanup waited %v for an unread stream", elapsed)
			}
			if n, err := io.Copy(io.Discard, stream); err == nil || n >= meta.Size {
				t.Fatalf("stream after cleanup read %d bytes, error %v", n, err)
			}
		})
	}
}

// TestPeerFetchThroughProxyEndToEnd runs two proxies through run and a third
// peer node. The proxy trusts upstream servers through the system pool, which
// a process loads once, so the scenario runs in a child process whose pool
// holds only the local origin certificate.
func TestPeerFetchThroughProxyEndToEnd(t *testing.T) {
	if dir := os.Getenv("OCI_ACCELERATOR_TEST_PEER_E2E_DIR"); dir != "" {
		runPeerFetchEndToEnd(t, dir)
		return
	}
	dir := t.TempDir()
	writeOriginCertificate(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPeerFetchThroughProxyEndToEnd$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(),
		"OCI_ACCELERATOR_TEST_PEER_E2E_DIR="+dir,
		"SSL_CERT_FILE="+filepath.Join(dir, "origin.pem"),
		"SSL_CERT_DIR="+filepath.Join(dir, "no-system-roots"),
	)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("end-to-end child failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "--- PASS: TestPeerFetchThroughProxyEndToEnd") {
		t.Fatalf("end-to-end child did not run the scenario:\n%s", output)
	}
}

func runPeerFetchEndToEnd(t *testing.T, dir string) {
	root := t.TempDir()
	const secret = "Bearer peer-e2e-registry-credential"
	body := peerTestBody(4 << 20)
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	blobPath := "/v2/library/app/blobs/" + digest

	origin := newPeerTestOrigin(t, dir, secret, blobPath, digest, body)
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	serverTLS, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}

	proxyA, proxyB := newTestListener(t), newTestListener(t)
	metricsA := newTestListener(t)
	peerA, peerB, peerC := newTestListener(t), newTestListener(t), newTestListener(t)
	recorder := newPeerRecorder(t, serverTLS, clientTLS, peerB.Addr().String())
	addressA, addressB, addressC := peerA.Addr().String(), recorder.address, peerC.Addr().String()

	writeEnv := func(name, listen, metrics, peerListen, advertise string, seeds ...string) string {
		path := filepath.Join(root, name+".env")
		contents := fmt.Sprintf("OCI_ACCELERATOR_LISTEN=%s\nOCI_ACCELERATOR_REGISTRIES=%s\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_PEER_LISTEN=%s\nOCI_ACCELERATOR_PEER_ADVERTISE=%s\nOCI_ACCELERATOR_PEER_SEEDS=%s\nOCI_ACCELERATOR_PEER_STATE_DIR=%s\nOCI_ACCELERATOR_SHUTDOWN_TIMEOUT=2s\n",
			listen, origin.authority, filepath.Join(root, "ca"), filepath.Join(root, "cache-"+name), peerListen, advertise, strings.Join(seeds, ","), filepath.Join(root, name))
		if metrics != "" {
			contents += "OCI_ACCELERATOR_METRICS_LISTEN=" + metrics + "\n"
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stopA := startRun(t, writeEnv("a", proxyA.Addr().String(), metricsA.Addr().String(), addressA, addressA, addressB, addressC), proxyA, metricsA, peerA)
	stopB := startRun(t, writeEnv("b", proxyB.Addr().String(), "", peerB.Addr().String(), addressB, addressA, addressC), proxyB, peerB)
	c := startPeerNode(t, root, signer, "c", peerC, []string{addressA, addressB}, openPeerTestStore(t, filepath.Join(root, "cache-c")), 30*time.Second)

	idA, idB := waitForNodeID(t, filepath.Join(root, "a")), waitForNodeID(t, filepath.Join(root, "b"))
	waitForPeers(t, filepath.Join(root, "a"), map[string]string{addressB: idB, addressC: c.id})
	waitForPeers(t, filepath.Join(root, "b"), map[string]string{addressA: idA, addressC: c.id})
	waitForPeers(t, filepath.Join(root, "c"), map[string]string{addressA: idA, addressB: idB})

	// B fills from the origin, and its hint reaches A and C.
	if got := proxyGet(t, signer, proxyB.Addr().String(), origin.url+blobPath, secret); !bytes.Equal(got, body) {
		t.Fatalf("B returned %d bytes, want the %d-byte blob", len(got), len(body))
	}
	waitForMetric(t, metricsA.Addr().String(), "oci_accelerator_availability_hints 1\n")

	// The origin authorizes A's request but then stalls its body, so only
	// B's copy can complete the response.
	if got := proxyGet(t, signer, proxyA.Addr().String(), origin.url+blobPath, secret); !bytes.Equal(got, body) {
		t.Fatalf("A returned %d bytes, want the %d-byte blob", len(got), len(body))
	}
	if authorized, rejected := origin.authorized.Load(), origin.rejected.Load(); authorized != 2 || rejected != 0 {
		t.Fatalf("origin saw %d authorized and %d unauthorized blob GETs, want 2 and 0", authorized, rejected)
	}
	fetches := recorder.fetches(t, secret)
	if fetches != 1 {
		t.Fatalf("B's peer listener served %d fetches, want 1", fetches)
	}

	// C learns A's ownership after A commits the peer copy.
	deadline := time.Now().Add(20 * time.Second)
	for c.availability.Stats().Hints < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("C stats = %+v, want hints from A and B", c.availability.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertFetchRejectedAtTLS(t, root, signer, addressA, idA, cache.Metadata{Key: strings.Repeat("0", 64), Digest: digest, Size: int64(len(body)), MediaType: "application/octet-stream"})

	if err := stopA(); err != nil {
		t.Fatal(err)
	}
	if err := stopB(); err != nil {
		t.Fatal(err)
	}
	storeA := openPeerTestStore(t, filepath.Join(root, "cache-a"))
	keys := storeA.Keys()
	if len(keys) != 1 {
		t.Fatalf("A cached keys = %v, want the blob", keys)
	}
	meta := cache.Metadata{Key: keys[0], Digest: digest, Size: int64(len(body)), MediaType: "application/octet-stream"}
	assertCachedBytes(t, storeA, meta, body)
	if got, want := c.availability.Owners(keys[0]), sortedIDs(idA, idB); !reflect.DeepEqual(got, want) {
		t.Fatalf("C owners = %v, want %v", got, want)
	}
	if fetches := recorder.fetches(t, secret); fetches != 1 {
		t.Fatalf("B's peer listener served %d fetches after shutdown, want 1", fetches)
	}
}

// peerNode is one startPeering instance with its store.
type peerNode struct {
	address      string
	id           string
	store        *cache.Store
	availability *peering.Availability
	transfer     *peering.Transfer
	stop         func() error
}

// startPeerMesh starts three nodes that seed each other and waits until every
// book resolves every peer, so a commit's first availability pass reaches all
// of them.
func startPeerMesh(t *testing.T, root string, signer *ca.Authority, transferTimeout time.Duration) []peerNode {
	t.Helper()
	names := []string{"a", "b", "c"}
	listeners := []net.Listener{newTestListener(t), newTestListener(t), newTestListener(t)}
	nodes := make([]peerNode, len(names))
	for i, name := range names {
		var seeds []string
		for j, listener := range listeners {
			if j != i {
				seeds = append(seeds, listener.Addr().String())
			}
		}
		nodes[i] = startPeerNode(t, root, signer, name, listeners[i], seeds, openPeerTestStore(t, filepath.Join(root, "cache-"+name)), transferTimeout)
	}
	for i, name := range names {
		want := map[string]string{}
		for j, other := range nodes {
			if j != i {
				want[other.address] = other.id
			}
		}
		waitForPeers(t, filepath.Join(root, name), want)
	}
	return nodes
}

func startPeerNode(t *testing.T, root string, signer *ca.Authority, name string, listener net.Listener, seeds []string, store *cache.Store, transferTimeout time.Duration) peerNode {
	t.Helper()
	address := listener.Addr().String()
	cfg := peerTestConfig(address, address, filepath.Join(root, name), seeds)
	cfg.Cache.FillTimeout = transferTimeout
	cfg.ShutdownTimeout = time.Minute
	_, availability, transfer, cleanup, err := startPeering(context.Background(), cfg, signer, store, slog.New(slog.NewTextHandler(io.Discard, nil)), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceValue(cleanup)
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	return peerNode{address: address, id: readNodeID(t, filepath.Join(root, name)), store: store, availability: availability, transfer: transfer, stop: stop}
}

// startRun serves run on listeners until the returned stop cancels it.
func startRun(t *testing.T, envFile string, listeners ...net.Listener) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--env-file", envFile}, io.Discard, io.Discard, listenForTest(listeners...))
	}()
	stop := sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitForOwners(t *testing.T, availability *peering.Availability, key string, want []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !reflect.DeepEqual(availability.Owners(key), want) {
		if time.Now().After(deadline) {
			t.Fatalf("owners = %v, want %v", availability.Owners(key), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForNodeID(t *testing.T, state string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id, err := os.ReadFile(filepath.Join(state, "node-id")); err == nil && len(id) != 0 {
			return string(id)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no node ID in %s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForMetric(t *testing.T, address, line string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if response, err := client.Get("http://" + address + "/metrics"); err == nil {
			encoded, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if strings.Contains(string(encoded), line) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics never reported %q", line)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertFetchRejectedAtTLS shows that clients without a fleet certificate
// never reach the fetch handler at address.
func assertFetchRejectedAtTLS(t *testing.T, root string, signer *ca.Authority, address, recipient string, meta cache.Metadata) {
	t.Helper()
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := clientTLS.Clone()
	unauthenticated.GetClientCertificate = nil
	foreign := openPeerTestCA(t, filepath.Join(root, fmt.Sprintf("foreign-ca-%d", time.Now().UnixNano())))
	_, foreignTLS, err := foreign.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	foreignTLS.RootCAs = clientTLS.RootCAs
	request := fmt.Sprintf(`{"recipientNodeId":%q,"key":%q,"digest":%q,"size":%d,"mediaType":%q}`, recipient, meta.Key, meta.Digest, meta.Size, meta.MediaType)
	for name, config := range map[string]*tls.Config{"unauthenticated": unauthenticated, "wrong CA": foreignTLS} {
		client := peering.NewClient(config)
		response, err := client.Post("https://"+address+"/v1/cache/fetch", "application/json", strings.NewReader(request))
		client.CloseIdleConnections()
		if err == nil {
			response.Body.Close()
			t.Fatalf("%s fetch reached HTTP with status %d", name, response.StatusCode)
		}
	}
}

func assertCachedBytes(t *testing.T, store *cache.Store, meta cache.Metadata, want []byte) {
	t.Helper()
	reader, err := store.Get(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("cached entry has %d bytes, error %v, want the %d committed bytes", len(got), err, len(want))
	}
}

// proxyGet fetches target through the proxy the way a registry client does,
// trusting the proxy CA for the intercepted registry.
func proxyGet(t *testing.T, signer *ca.Authority, proxyAddress, target, authorization string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(signer.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(encoded) {
		t.Fatal("cannot parse proxy CA certificate")
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddress}), TLSClientConfig: &tls.Config{RootCAs: roots}, DisableCompression: true},
		Timeout:   30 * time.Second,
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("proxy GET status %d, %d bytes, error %v", response.StatusCode, len(got), err)
	}
	return got
}

// peerTestOrigin is a local TLS registry that requires a bearer credential.
// It serves the blob body to the first authorized GET only. Later authorized
// GETs receive complete headers and then a body that never arrives.
type peerTestOrigin struct {
	url, authority       string
	authorized, rejected atomic.Int32
}

func newPeerTestOrigin(t *testing.T, dir, secret, blobPath, digest string, body []byte) *peerTestOrigin {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "origin.pem"), filepath.Join(dir, "origin-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	origin := &peerTestOrigin{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != blobPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != secret {
			origin.rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		header := w.Header()
		header.Set("Content-Type", "application/octet-stream")
		header.Set("Content-Length", fmt.Sprint(len(body)))
		header.Set("Docker-Content-Digest", digest)
		if origin.authorized.Add(1) == 1 {
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	origin.url = server.URL
	origin.authority = strings.TrimPrefix(server.URL, "https://")
	return origin
}

// writeOriginCertificate writes a self-signed certificate for 127.0.0.1 that
// serves as the child process's only trusted root.
func writeOriginCertificate(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "peer e2e origin"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "origin.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "origin-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "no-system-roots"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// peerRecorder terminates fleet mutual TLS in front of a peer listener and
// records every decrypted request before forwarding it over fleet TLS.
type peerRecorder struct {
	address  string
	mu       sync.Mutex
	requests []recordedPeerRequest
}

type recordedPeerRequest struct {
	path   string
	header http.Header
	body   []byte
}

func newPeerRecorder(t *testing.T, serverTLS, clientTLS *tls.Config, target string) *peerRecorder {
	t.Helper()
	listener := newTestListener(t)
	recorder := &peerRecorder{address: listener.Addr().String()}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "https", Host: target})
		},
		Transport: peering.NewClient(clientTLS).Transport,
		ErrorLog:  log.New(io.Discard, "", 0),
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			if err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			recorder.mu.Lock()
			recorder.requests = append(recorder.requests, recordedPeerRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
			recorder.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			forward.ServeHTTP(w, r)
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = server.Serve(tls.NewListener(listener, serverTLS)) }()
	t.Cleanup(func() { _ = server.Close() })
	return recorder
}

// fetches counts recorded fetch requests and fails if any recorded request
// carried a registry credential.
func (r *peerRecorder) fetches(t *testing.T, secret string) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, request := range r.requests {
		if len(request.header.Values("Authorization")) != 0 || bytes.Contains(request.body, []byte(secret)) {
			t.Fatalf("peer request to %s carried a registry credential", request.path)
		}
		for _, values := range request.header {
			for _, value := range values {
				if strings.Contains(value, secret) {
					t.Fatalf("peer request to %s carried a registry credential", request.path)
				}
			}
		}
		if request.path == "/v1/cache/fetch" {
			count++
		}
	}
	return count
}

// peerTestBody returns deterministic, incompressible-looking bytes.
func peerTestBody(size int) []byte {
	body := make([]byte, 0, size+sha256.Size)
	block := sha256.Sum256([]byte("peer fetch body"))
	for len(body) < size {
		body = append(body, block[:]...)
		block = sha256.Sum256(block[:])
	}
	return body[:size]
}

func sortedIDs(ids ...string) []string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return ids
}
