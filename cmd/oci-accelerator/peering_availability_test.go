package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/config"
	"github.com/buildkite/oci-accelerator/internal/peering"
)

const availabilityTestSelf = "self.test:7443"

func TestPeerAvailabilityAnnouncesCommittedFillsWithConfiguredOwner(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	store := openPeerTestStore(t)
	remote := startCapturePeer(t, signer)
	listener := newTestListener(t)
	state := filepath.Join(root, "peer")
	cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: availabilityTestSelf, Seeds: []string{remote.address}, StateDir: state}, ShutdownTimeout: time.Second}
	done, cleanup, err := startPeering(context.Background(), cfg, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), store, listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	aborted := testObject("aborted body")
	fill, err := store.Begin(aborted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write([]byte("abort")); err != nil {
		t.Fatal(err)
	}
	if err := fill.Abort(); err != nil {
		t.Fatal(err)
	}
	failed := testObject("failed body")
	fill, err = store.Begin(failed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write([]byte("corrupt bod")); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err == nil {
		t.Fatal("corrupt fill committed")
	}
	committed := testObject("committed body")
	commitTestObject(t, store, committed, "committed body")

	message, claim := remote.waitForClaim(t, committed.Key)
	if message.Sender != availabilityTestSelf || claim.Owner != availabilityTestSelf || !claim.Present || claim.Revision == 0 || len(claim.Session) != 32 {
		t.Fatalf("announcement = sender %q, claim %#v", message.Sender, claim)
	}
	if !slices.Contains(claim.Seen, availabilityTestSelf) {
		t.Fatalf("announcement seen %v does not include origin", claim.Seen)
	}
	if expires := time.UnixMilli(claim.Expires); expires.Before(time.Now()) || expires.After(time.Now().Add(11*time.Minute)) {
		t.Fatalf("announcement expires at %v", expires)
	}
	// The committed key's sync took a full inventory snapshot, so aborted or
	// failed keys present in the store would have been sent no later than it.
	for _, key := range []string{aborted.Key, failed.Key} {
		if remote.sawKey(key) {
			t.Fatalf("unpublished fill %s was announced", key)
		}
	}

	// Hold the next announcement open so cleanup must cancel it.
	remote.hold()
	commitTestObject(t, store, testObject("held body"), "held body")
	select {
	case <-remote.held:
	case <-time.After(5 * time.Second):
		t.Fatal("second announcement did not reach the remote peer")
	}
	cleaned := make(chan error, 1)
	go func() { cleaned <- cleanup() }()
	select {
	case err := <-cleaned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not cancel the in-flight announcement")
	}
	select {
	case <-remote.released:
	case <-time.After(5 * time.Second):
		t.Fatal("remote peer request was not cancelled by cleanup")
	}
	select {
	case <-done:
	default:
		t.Fatal("serve completion channel remains open after cleanup")
	}
	requests := remote.availabilityRequests()
	time.Sleep(1500 * time.Millisecond) // longer than one jittered availability interval
	if got := remote.availabilityRequests(); got != requests {
		t.Fatalf("availability requests after cleanup = %d, want %d", got, requests)
	}
	assertListenerClosed(t, listener)
	book, err := peering.OpenBook(state, cfg.Peer.Advertise)
	if err != nil {
		t.Fatalf("state lock not released: %v", err)
	}
	book.Close()
}

func TestPeerAvailabilityEndpointAcceptsOnlyValidFleetAnnouncements(t *testing.T) {
	root := t.TempDir()
	signer := openPeerTestCA(t, filepath.Join(root, "ca"))
	remote := startCapturePeer(t, signer)
	listener := newTestListener(t)
	cfg := config.Config{Peer: config.Peer{Listen: listener.Addr().String(), Advertise: availabilityTestSelf, Seeds: []string{remote.address}, StateDir: filepath.Join(root, "peer")}, ShutdownTimeout: time.Second}
	_, cleanup, err := startPeering(context.Background(), cfg, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), openPeerTestStore(t), listenForTest(listener))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	_, clientTLS, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	client := peering.NewClient(clientTLS)
	defer client.CloseIdleConnections()
	post := func(body string) int {
		t.Helper()
		resp, err := client.Post("https://"+listener.Addr().String()+"/v1/cache/availability", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	const origin = "origin.test:7443"
	malformedKey, unseenSenderKey, unknownFieldKey := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	malformed := map[string]string{
		"invalid JSON":  `{"sender":`,
		"unknown field": strings.Replace(availabilityTestBody(t, origin, origin, unknownFieldKey), `{"sender"`, `{"extra":1,"sender"`, 1),
		"trailing data": availabilityTestBody(t, origin, origin, malformedKey) + "{}",
		// The sender is absent from the claim's seen set.
		"unseen sender": strings.Replace(availabilityTestBody(t, origin, origin, unseenSenderKey), `"sender":"origin.test:7443"`, `"sender":"other.test:7443"`, 1),
	}
	for name, body := range malformed {
		if status := post(body); status != http.StatusBadRequest {
			t.Fatalf("%s announcement status = %d, want %d", name, status, http.StatusBadRequest)
		}
	}
	validKey := strings.Repeat("4", 64)
	if status := post(availabilityTestBody(t, origin, origin, validKey)); status != http.StatusNoContent {
		t.Fatalf("valid announcement status = %d, want %d", status, http.StatusNoContent)
	}

	// Relay to the seed proves the accepted claim reached the live index.
	message, claim := remote.waitForClaim(t, validKey)
	if message.Sender != availabilityTestSelf || claim.Owner != origin || !claim.Present {
		t.Fatalf("relayed announcement = sender %q, claim %#v", message.Sender, claim)
	}
	if !slices.Contains(claim.Seen, origin) || !slices.Contains(claim.Seen, availabilityTestSelf) {
		t.Fatalf("relayed seen %v lacks origin or relay", claim.Seen)
	}
	for _, key := range []string{malformedKey, unseenSenderKey, unknownFieldKey} {
		if remote.sawKey(key) {
			t.Fatalf("rejected announcement %s was relayed", key)
		}
	}
}

func testObject(body string) cache.Metadata {
	digest := sha256.Sum256([]byte(body))
	key := sha256.Sum256([]byte("key:" + body))
	return cache.Metadata{Key: hex.EncodeToString(key[:]), Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(body)), MediaType: "application/octet-stream"}
}

func commitTestObject(t *testing.T, store *cache.Store, meta cache.Metadata, body string) {
	t.Helper()
	fill, err := store.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fill.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
}

type availabilityTestMessage struct {
	Sender string          `json:"sender"`
	Claims []peering.Claim `json:"claims"`
}

// capturePeer is a remote fleet peer that records availability pushes and
// rejects discovery exchanges.
type capturePeer struct {
	address  string
	held     chan struct{}
	released chan struct{}

	mu       sync.Mutex
	messages []availabilityTestMessage
	holding  bool
}

func startCapturePeer(t *testing.T, signer *ca.Authority) *capturePeer {
	t.Helper()
	serverTLS, _, err := signer.PeerTLSConfigs()
	if err != nil {
		t.Fatal(err)
	}
	listener := newTestListener(t)
	peer := &capturePeer{address: listener.Addr().String(), held: make(chan struct{}), released: make(chan struct{})}
	server := &http.Server{
		Handler:      http.HandlerFunc(peer.serveHTTP),
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		ErrorLog:     log.New(io.Discard, "", 0),
	}
	served := make(chan struct{})
	go func() {
		_ = server.Serve(tls.NewListener(listener, serverTLS))
		close(served)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-served
	})
	return peer
}

func (p *capturePeer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/cache/availability" || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "unsupported", http.StatusNotFound)
		return
	}
	var message availabilityTestMessage
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.messages = append(p.messages, message)
	holding := p.holding
	p.holding = false
	p.mu.Unlock()
	if holding {
		close(p.held)
		<-r.Context().Done()
		close(p.released)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hold makes the next availability request block until its client goes away.
func (p *capturePeer) hold() {
	p.mu.Lock()
	p.holding = true
	p.mu.Unlock()
}

func (p *capturePeer) availabilityRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.messages)
}

func (p *capturePeer) sawKey(key string) bool {
	_, _, ok := p.find(key)
	return ok
}

func (p *capturePeer) waitForClaim(t *testing.T, key string) (availabilityTestMessage, peering.Claim) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if message, claim, ok := p.find(key); ok {
			return message, claim
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote peer did not receive a claim for %s", key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *capturePeer) find(key string) (availabilityTestMessage, peering.Claim, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, message := range p.messages {
		for _, claim := range message.Claims {
			if claim.Key == key {
				return message, claim, true
			}
		}
	}
	return availabilityTestMessage{}, peering.Claim{}, false
}
