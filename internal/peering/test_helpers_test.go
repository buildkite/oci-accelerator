package peering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testIDA = "nodeA-ID_123"
	testIDB = "nodeB-ID_456"
	testIDC = "nodeC-ID_789"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testNodeID returns a valid node ID that is distinct for each i.
func testNodeID(i int) string { return fmt.Sprintf("%012d", i) }

func bookTestDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "book")
}

func cooldownDiscovery(t *testing.T, transport roundTripFunc) *Discovery {
	t.Helper()
	book, err := OpenBook(bookTestDir(t), "b.test:2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return NewDiscovery(book, &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func cooldownRequest(d *Discovery, message Message) *httptest.ResponseRecorder {
	if message.Peers == nil {
		message.Peers = map[string]string{}
	}
	body, _ := json.Marshal(message)
	request := httptest.NewRequest(http.MethodPost, "/v1/peers/exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, request)
	return response
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func mustNodeID(t *testing.T, s string) nodeID {
	t.Helper()
	id, ok := parseNodeID(s)
	if !ok {
		t.Fatalf("invalid test node ID %q", s)
	}
	return id
}

const testKeyText = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// claimFor returns a wire claim owned by testIDA and relayed by testIDB.
func claimFor(i int) string {
	return fmt.Sprintf(`{"ownerNodeId":"nodeA-ID_123","key":"%s","seen":["nodeA-ID_123","nodeB-ID_456"]}`, testCacheKey(i))
}

// testCacheKey returns a cache key that is distinct for each i.
func testCacheKey(i int) cacheKey {
	var key cacheKey
	key[0], key[1], key[2], key[3] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)
	return key
}

func testClaim(t *testing.T, owner string, key cacheKey, seen ...string) hintClaim {
	t.Helper()
	ids := make([]nodeID, len(seen))
	for i, s := range seen {
		ids[i] = mustNodeID(t, s)
	}
	slices.SortFunc(ids, compareNodeIDs)
	return hintClaim{owner: mustNodeID(t, owner), key: key, seen: ids}
}

// testInventory is a controllable cache inventory.
type testInventory struct {
	mu      sync.Mutex
	keys    []string
	changes chan struct{}
}

func newTestInventory(keys ...string) *testInventory {
	return &testInventory{keys: keys, changes: make(chan struct{}, 1)}
}

func (i *testInventory) Keys() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.keys)
}

func (i *testInventory) Changes() <-chan struct{} { return i.changes }

func (i *testInventory) set(keys ...string) {
	i.mu.Lock()
	i.keys = keys
	i.mu.Unlock()
	select {
	case i.changes <- struct{}{}:
	default:
	}
}

// newTestAvailability opens a book with a fixed node ID and an availability
// with a seeded random source and the given clock. Its send fails until the
// test replaces it.
func newTestAvailability(t *testing.T, id, address string, now func() time.Time, seed uint64) (*Availability, *testInventory) {
	t.Helper()
	dir := bookTestDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, nodeIDName), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := OpenBook(dir, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	inventory := newTestInventory()
	a, err := NewAvailability(book, inventory, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.now = now
	a.random = rand.New(rand.NewPCG(seed, 0))
	return a, inventory
}

// testFleet runs availability nodes in lockstep on one fake clock. Each step
// dispatches every node's due work and delivers it synchronously through the
// target's real handler, so requests use the wire codec.
type testFleet struct {
	t         *testing.T
	now       time.Time
	nodes     []*fleetNode
	byName    map[string]*fleetNode
	byAddress map[string]*fleetNode
	sent      []fleetRequest
	// fault, when set, decides whether a request reaches its target and
	// whether the sender then sees a failure.
	fault func(fleetRequest) (deliver bool, fail bool)
}

type fleetNode struct {
	name, id, address string
	a                 *Availability
	inventory         *testInventory
}

type fleetRequest struct {
	from, to  string // node names, or the address when no node listens there
	recipient string // node ID the sender addressed
	claims    int
}

func newTestFleet(t *testing.T, seed uint64, names ...string) *testFleet {
	t.Helper()
	f := &testFleet{t: t, now: time.Unix(1_000_000, 0), byName: map[string]*fleetNode{}, byAddress: map[string]*fleetNode{}}
	for i, name := range names {
		n := &fleetNode{name: name, id: testNodeID(i + 1), address: strings.ToLower(name) + ".test:443"}
		n.a, n.inventory = newTestAvailability(t, n.id, n.address, func() time.Time { return f.now }, seed+uint64(i))
		n.a.send = func(_ context.Context, address string, message availabilityMessage) error {
			return f.deliver(n, address, message)
		}
		f.nodes = append(f.nodes, n)
		f.byName[name] = n
		f.byAddress[n.address] = n
	}
	return f
}

// know records peers in from's address book under their current addresses.
func (f *testFleet) know(from string, peers ...string) {
	f.t.Helper()
	for _, name := range peers {
		f.bind(from, f.byName[name].address, f.byName[name].id)
	}
}

// bind records an address-to-ID binding in from's book, as discovery would.
func (f *testFleet) bind(from, address, id string) {
	f.t.Helper()
	if _, err := f.byName[from].a.book.Merge(Message{Address: address, NodeID: id, Peers: map[string]string{}}); err != nil {
		f.t.Fatal(err)
	}
}

// originate makes key part of name's cache inventory.
func (f *testFleet) originate(name string, key cacheKey) {
	n := f.byName[name]
	n.inventory.set(append(n.inventory.Keys(), key.String())...)
	n.a.reconcile(n.inventory.Keys())
}

// step advances past every cadence deadline, then dispatches and delivers each
// node's due work in order. It returns the number of requests sent.
func (f *testFleet) step() int {
	f.now = f.now.Add(2 * availabilityCadence)
	before := len(f.sent)
	for _, n := range f.nodes {
		for _, batch := range n.a.dispatch(availabilityConcurrency) {
			n.a.complete(batch, n.a.send(context.Background(), batch.to.address, batch.message))
		}
	}
	return len(f.sent) - before
}

// settle steps until no node has queued work, long enough for any backoff.
func (f *testFleet) settle() {
	f.t.Helper()
	for range 200 {
		f.step()
		idle := true
		for _, n := range f.nodes {
			if len(n.a.index.queue) != 0 {
				idle = false
			}
		}
		if idle {
			return
		}
	}
	f.t.Fatal("fleet did not settle")
}

func (f *testFleet) deliver(from *fleetNode, address string, message availabilityMessage) error {
	request := fleetRequest{from: from.name, to: address, recipient: message.recipient.String(), claims: len(message.claims)}
	target := f.byAddress[address]
	if target != nil {
		request.to = target.name
	}
	f.sent = append(f.sent, request)
	deliver, fail := true, false
	if f.fault != nil {
		deliver, fail = f.fault(request)
	}
	if target == nil || !deliver {
		return errAvailabilitySend
	}
	body, err := encodeAvailability(message)
	if err != nil {
		f.t.Fatal(err)
	}
	response := serveAvailability(target.a.Handler(), http.MethodPost, availabilityPath, "application/json", string(body))
	if fail || response.Code != http.StatusNoContent {
		return errAvailabilitySend
	}
	return nil
}

// hint returns name's retained record for owner's key, or nil.
func (f *testFleet) hint(name, owner string, key cacheKey) *hint {
	n := f.byName[name]
	n.a.mu.Lock()
	defer n.a.mu.Unlock()
	return n.a.index.find(mustNodeID(f.t, f.byName[owner].id), key)
}

// requests returns the requests sent from one node, in order.
func (f *testFleet) requests(from string) []fleetRequest {
	var out []fleetRequest
	for _, r := range f.sent {
		if r.from == from {
			out = append(out, r)
		}
	}
	return out
}

// seenNames returns the fleet names in a hint's seen set, sorted.
func (f *testFleet) seenNames(h *hint) []string {
	var names []string
	for _, n := range f.nodes {
		if containsNodeID(h.seen, mustNodeID(f.t, n.id)) {
			names = append(names, n.name)
		}
	}
	return names
}
