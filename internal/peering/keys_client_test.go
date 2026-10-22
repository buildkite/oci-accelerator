package peering

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
)

func TestFetchKeysLiveStoreMovementAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	store, err := cachetest.Open(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Commit in reverse lexical order so recency cannot be mistaken for key order.
	want := make([]cacheKey, 0, maxKeysPerPage+1)
	for i := maxKeysPerPage; i >= 0; i-- {
		key := testCacheKey(i)
		fill, err := store.Begin(cache.Metadata{Key: key.String(), Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", MediaType: "application/octet-stream"})
		if err != nil {
			t.Fatal(err)
		}
		if err := fill.Commit(); err != nil {
			t.Fatal(err)
		}
		want = append(want, key)
	}
	handler := newKeysHandler(testParsedNodeID(t, 2), store.KeyPage)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	var got []cacheKey
	err = fetchKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2), func(keys []cacheKey) {
		got = append(got, keys...)
		if len(got) == maxKeysPerPage {
			// Both an earlier key and the page boundary can reappear later.
			for _, key := range []cacheKey{want[0], want[maxKeysPerPage-1]} {
				reader, err := store.Get(context.Background(), cache.Metadata{Key: key.String(), Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", MediaType: "application/octet-stream"})
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	want = append(want, want[0], want[maxKeysPerPage-1])
	if err != nil || !reflect.DeepEqual(got, want) || requests.Load() != 2 {
		t.Fatalf("live fetch = %d keys, %d requests, %v, want %d keys over 2 requests", len(got), requests.Load(), err, len(want))
	}
	first := serveKeysPage(t, handler, keysPath)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := cachetest.Open(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedHandler := newKeysHandler(testParsedNodeID(t, 2), reopened.KeyPage)
	response := httptest.NewRecorder()
	reopenedHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, keysPath+"?after="+testCursor(t, first.next), nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("pre-restart cursor = HTTP %d, want 400", response.Code)
	}
	reopenedServer := httptest.NewTLSServer(reopenedHandler)
	defer reopenedServer.Close()
	got, err = collectKeys(context.Background(), reopenedServer.Client(), testServerAddress(reopenedServer), testParsedNodeID(t, 2))
	if err != nil || len(got) != maxKeysPerPage+1 {
		t.Fatalf("fresh fetch after restart = %d keys, %v", len(got), err)
	}
}

func TestFetchKeysPagesFullInventoryOverTLS(t *testing.T) {
	for size, wantRequests := range map[int]int32{maxKeysPerPage: 1, maxKeysPerPage + 1: 2, 2*maxKeysPerPage + 1: 3, maxHintRecords + 1: 49} {
		want := testCacheKeys(0, size)
		// Fetch order follows recency, even when it is reverse lexical order.
		for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
			want[i], want[j] = want[j], want[i]
		}
		inventory := testInventory(want)
		var requests atomic.Int32
		handler := newKeysHandler(testParsedNodeID(t, 2), testKeyPages(inventory))
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			handler.ServeHTTP(w, r)
		}))
		keys, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2))
		server.Close()
		if err != nil {
			t.Fatalf("%d keys: %v", size, err)
		}
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("%d keys: fetched %d keys, want all in order", size, len(keys))
		}
		if requests.Load() != wantRequests {
			t.Errorf("%d keys: requests = %d, want %d", size, requests.Load(), wantRequests)
		}
	}
}

func TestFetchKeysEmptyInventory(t *testing.T) {
	server := httptest.NewTLSServer(newKeysHandler(testParsedNodeID(t, 2), testKeyPages(nil)))
	defer server.Close()
	keys, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2))
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
}

func TestFetchKeysRejectsAnotherNode(t *testing.T) {
	// The address now belongs to node 3, so none of its keys count as node 2's.
	server := httptest.NewTLSServer(newKeysHandler(testParsedNodeID(t, 3), testKeyPages(testInventory(testCacheKeys(0, 1)))))
	defer server.Close()
	if keys, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2)); err == nil || len(keys) != 0 {
		t.Fatalf("fetch from another node returned %d keys", len(keys))
	}
}

func TestFetchKeysRejectsNodeChangeBetweenPages(t *testing.T) {
	inventory := testInventory(testCacheKeys(0, maxKeysPerPage+1))
	first := newKeysHandler(testParsedNodeID(t, 2), testKeyPages(inventory))
	later := newKeysHandler(testParsedNodeID(t, 3), testKeyPages(inventory))
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			first.ServeHTTP(w, r)
		} else {
			later.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	if keys, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2)); err == nil || len(keys) != maxKeysPerPage {
		t.Fatal("fetch succeeded after the node changed")
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestFetchKeysRejectsBadResponses(t *testing.T) {
	node := testNodeID(2)
	firstPosition := testKeyPosition(maxKeysPerPage, testCacheKey(maxKeysPerPage-1))
	firstPage, err := encodeKeysPage(keysPage{node: testParsedNodeID(t, 2), keys: testCacheKeys(0, maxKeysPerPage), next: firstPosition})
	if err != nil {
		t.Fatal(err)
	}
	badContinuation := func(position cache.KeyPosition) string {
		keys := testCacheKeys(0, maxKeysPerPage)
		keys[len(keys)-1], _ = parseCacheKey(position.Key)
		encoded, err := encodeKeysPage(keysPage{node: testParsedNodeID(t, 2), keys: keys, next: position})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	older := firstPosition
	older.Last--
	otherStore := firstPosition
	otherStore.Last++
	otherStore.Incarnation = [16]byte{2}
	jsonResponse := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
	}
	// pages serves the full first page, then body for the request after it.
	pages := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.RawQuery == "" {
				_, _ = w.Write(firstPage)
			} else {
				_, _ = io.WriteString(w, body)
			}
		})
	}
	tests := map[string]http.Handler{
		"status": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":""}`, node))
		}),
		"content type": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":""}`, node))
		}),
		"short page with more":   jsonResponse(fmt.Sprintf(`{"nodeId":%q,"keys":[%q],"next":%q}`, node, testCacheKey(1), testCacheKey(1))),
		"malformed":              pages(`{}`),
		"repeated page":          pages(string(firstPage)),
		"cursor moves backwards": pages(badContinuation(older)),
		"cursor changes store":   pages(badContinuation(otherStore)),
		"oversized second page":  pages(fmt.Sprintf(`{"nodeId":%q,"keys":[],"next":""}`, node) + strings.Repeat(" ", maxKeysPageBytes)),
	}
	for name, handler := range tests {
		server := httptest.NewTLSServer(handler)
		if keys, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2)); err == nil {
			t.Errorf("%s: fetch returned %d keys", name, len(keys))
		}
		server.Close()
	}
}

func TestFetchKeysKeepsAppliedPageWhenCanceled(t *testing.T) {
	inventory := testInventory(testCacheKeys(0, 2*maxKeysPerPage))
	handler := newKeysHandler(testParsedNodeID(t, 2), testKeyPages(inventory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	var applied []cacheKey
	err := fetchKeys(ctx, server.Client(), testServerAddress(server), testParsedNodeID(t, 2), func(keys []cacheKey) {
		applied = append(applied, keys...)
		cancel()
	})
	if err == nil || !reflect.DeepEqual(applied, testCacheKeys(0, maxKeysPerPage)) {
		t.Fatalf("canceled fetch = %d keys, %v", len(applied), err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestFetchKeysDoesNotFollowRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+keysPath, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), testParsedNodeID(t, 2)); err == nil {
		t.Fatal("fetch succeeded through a redirect")
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target requests = %d", targetRequests.Load())
	}
}

func TestFetchKeysRejectsInvalidInputsBeforeSending(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	if _, err := collectKeys(context.Background(), nil, testServerAddress(server), testParsedNodeID(t, 2)); err == nil {
		t.Error("nil client succeeded")
	}
	if _, err := collectKeys(context.Background(), server.Client(), "no-port", testParsedNodeID(t, 2)); err == nil {
		t.Error("invalid destination succeeded")
	}
	if _, err := collectKeys(context.Background(), server.Client(), testServerAddress(server), nodeID{}); err == nil {
		t.Error("invalid expected node succeeded")
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func testServerAddress(server *httptest.Server) string {
	return strings.TrimPrefix(server.URL, "https://")
}

// collectKeys retains delivered pages for assertions. Production applies each
// page without retaining the complete fetch.
func collectKeys(ctx context.Context, client *http.Client, address string, expected nodeID) ([]cacheKey, error) {
	var keys []cacheKey
	err := fetchKeys(ctx, client, address, expected, func(page []cacheKey) { keys = append(keys, page...) })
	return keys, err
}
