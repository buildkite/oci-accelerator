package peering

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestKeysHandlerPagesInventory(t *testing.T) {
	want := testCacheKeys(0, 2*maxKeysPerPage+1)
	// Lexical ordering must not replace the provider's supplied recency order.
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}
	handler := newKeysHandler(testParsedNodeID(t, 1), testKeyPages(testInventory(want)))
	first := serveKeysPage(t, handler, keysPath)
	assertKeysPage(t, first, testParsedNodeID(t, 1), want[:maxKeysPerPage], true)
	second := serveKeysPage(t, handler, keysPath+"?after="+testCursor(t, first.next))
	assertKeysPage(t, second, testParsedNodeID(t, 1), want[maxKeysPerPage:2*maxKeysPerPage], true)
	last := serveKeysPage(t, handler, keysPath+"?after="+testCursor(t, second.next))
	assertKeysPage(t, last, testParsedNodeID(t, 1), want[2*maxKeysPerPage:], false)
}

func TestKeysHandlerEndsWithFullPage(t *testing.T) {
	handler := newKeysHandler(testParsedNodeID(t, 1), testKeyPages(testInventory(testCacheKeys(0, 2*maxKeysPerPage))))
	first := serveKeysPage(t, handler, keysPath)
	assertKeysPage(t, first, testParsedNodeID(t, 1), testCacheKeys(0, maxKeysPerPage), true)
	last := serveKeysPage(t, handler, keysPath+"?after="+testCursor(t, first.next))
	assertKeysPage(t, last, testParsedNodeID(t, 1), testCacheKeys(maxKeysPerPage, maxKeysPerPage), false)
}

func TestKeysHandlerRejectsPreviousStoreIncarnation(t *testing.T) {
	handler := newKeysHandler(testParsedNodeID(t, 1), testKeyPages(nil))
	after := testKeyPosition(1, testCacheKey(1))
	after.Incarnation = [16]byte{2}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, keysPath+"?after="+testCursor(t, after), nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("previous incarnation = %d, want 400", response.Code)
	}
	if got := response.Header().Get("OCI-Accelerator-Error"); got != "invalid_cursor" {
		t.Fatalf("cursor error code = %q, want invalid_cursor", got)
	}
}

func TestKeysHandlerFailsClosedOnInvalidInventory(t *testing.T) {
	k1, k2 := testCacheKey(1).String(), testCacheKey(2).String()
	tests := map[string]struct {
		self      nodeID
		inventory []string
	}{
		"invalid key":  {testParsedNodeID(t, 1), []string{k1, "not-a-key", k2}},
		"uppercase":    {testParsedNodeID(t, 1), []string{k1, strings.Repeat("AB", 32)}},
		"duplicate":    {testParsedNodeID(t, 1), []string{k1, k1}},
		"invalid self": {nodeID{}, []string{k1}},
	}
	for name, test := range tests {
		handler := newKeysHandler(test.self, testKeyPages(test.inventory))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, keysPath, nil))
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), k1) {
			t.Errorf("%s: status = %d, body length = %d", name, response.Code, response.Body.Len())
		}
	}
}

func TestKeysHandlerRejectsInvalidRequestsBeforeInventory(t *testing.T) {
	calls := 0
	handler := newKeysHandler(testParsedNodeID(t, 1), func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error) {
		calls++
		return nil, cache.KeyPosition{}, nil
	})
	token := testCursor(t, testKeyPosition(1, testCacheKey(1)))
	tests := map[string]*http.Request{
		"post":             httptest.NewRequest(http.MethodPost, keysPath, nil),
		"body":             httptest.NewRequest(http.MethodGet, keysPath, strings.NewReader("{}")),
		"other path":       httptest.NewRequest(http.MethodGet, "/v1/cache/keys/", nil),
		"empty cursor":     httptest.NewRequest(http.MethodGet, keysPath+"?after=", nil),
		"old key cursor":   httptest.NewRequest(http.MethodGet, keysPath+"?after="+testCacheKey(1).String(), nil),
		"unknown version":  httptest.NewRequest(http.MethodGet, keysPath+"?after="+strings.Repeat("A", 76), nil),
		"invalid position": httptest.NewRequest(http.MethodGet, keysPath+"?after=AQ"+strings.Repeat("A", 74), nil),
		"short cursor":     httptest.NewRequest(http.MethodGet, keysPath+"?after=abc", nil),
		"other parameter":  httptest.NewRequest(http.MethodGet, keysPath+"?before="+token, nil),
		"extra parameter":  httptest.NewRequest(http.MethodGet, keysPath+"?after="+token+"&limit=1", nil),
		"bare query":       httptest.NewRequest(http.MethodGet, keysPath+"?", nil),
	}
	for name, request := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want %d", name, response.Code, http.StatusBadRequest)
		}
		if got := response.Header().Get("OCI-Accelerator-Error"); got != "" {
			t.Errorf("%s: unexpected cursor error code %q", name, got)
		}
	}
	if calls != 0 {
		t.Fatalf("inventory calls = %d, want 0", calls)
	}
}

func serveKeysPage(t *testing.T, handler http.Handler, target string) keysPage {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s: status = %d, content type = %q", target, response.Code, response.Header().Get("Content-Type"))
	}
	page, err := decodeKeysPage(response.Body.Bytes())
	if err != nil {
		t.Fatalf("GET %s: decode: %v", target, err)
	}
	return page
}

func assertKeysPage(t *testing.T, page keysPage, node nodeID, keys []cacheKey, more bool) {
	t.Helper()
	if page.node != node || !reflect.DeepEqual(page.keys, keys) || (page.next != (cache.KeyPosition{})) != more {
		t.Fatalf("page = node %s, %d keys, next %+v, want node %s, %d keys, more %t", page.node, len(page.keys), page.next, node, len(keys), more)
	}
}
