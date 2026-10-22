package peering

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
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

// testParsedNodeID returns testNodeID(i) in parsed form.
func testParsedNodeID(t *testing.T, i int) nodeID {
	t.Helper()
	id, ok := parseNodeID(testNodeID(i))
	if !ok {
		t.Fatalf("invalid test node ID %q", testNodeID(i))
	}
	return id
}

// testCacheKey returns a distinct cache key for i that sorts by i.
func testCacheKey(i int) cacheKey {
	var key cacheKey
	binary.BigEndian.PutUint64(key[:8], uint64(i))
	return key
}

// testInventory returns keys in their text form, as a cache inventory lists
// them.
func testInventory(keys []cacheKey) []string {
	inventory := make([]string, len(keys))
	for i, key := range keys {
		inventory[i] = key.String()
	}
	return inventory
}

// testKeyPages assigns stable logical positions to a fixed ordered inventory.
// Live cache movement is exercised through real stores in the cache tests.
func testKeyPages(keys []string) func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error) {
	return func(after cache.KeyPosition, limit int) ([]string, cache.KeyPosition, error) {
		if after != (cache.KeyPosition{}) && after.Incarnation != ([16]byte{1}) {
			return nil, cache.KeyPosition{}, cache.ErrKeyCursor
		}
		start := min(int(after.Last), len(keys))
		end := min(start+limit, len(keys))
		var next cache.KeyPosition
		if end < len(keys) {
			next = cache.KeyPosition{Incarnation: [16]byte{1}, Last: int64(end), Key: keys[end-1]}
		}
		return keys[start:end], next, nil
	}
}

func testKeyPosition(last int, key cacheKey) cache.KeyPosition {
	return cache.KeyPosition{Incarnation: [16]byte{1}, Last: int64(last), Key: key.String()}
}

func testCursor(t *testing.T, position cache.KeyPosition) string {
	t.Helper()
	token, err := encodeKeysCursor(position)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// testCacheKeys returns n ascending keys starting at testCacheKey(first).
func testCacheKeys(first, n int) []cacheKey {
	keys := make([]cacheKey, n)
	for i := range keys {
		keys[i] = testCacheKey(first + i)
	}
	return keys
}

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
