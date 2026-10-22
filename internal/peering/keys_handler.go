package peering

import (
	"errors"
	"net/http"
	"strings"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// newKeysHandler serves live LRU-to-MRU pages from the local cache. inventory
// owns ordering and store lifetime, while this handler validates requests and
// encodes the wire cursor. A listing grants no access to cached objects.
func newKeysHandler(self nodeID, inventory func(cache.KeyPosition, int) ([]string, cache.KeyPosition, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, ok := parseKeysRequest(r)
		if !ok {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		keys, next, err := inventory(after, maxKeysPerPage)
		if errors.Is(err, cache.ErrKeyCursor) {
			w.Header().Set(keysErrorHeader, "invalid_cursor")
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		if err != nil || len(keys) > maxKeysPerPage {
			http.Error(w, "cache keys unavailable", http.StatusInternalServerError)
			return
		}
		page := keysPage{node: self, keys: make([]cacheKey, len(keys)), next: next}
		for i, text := range keys {
			if page.keys[i], ok = parseCacheKey(text); !ok {
				http.Error(w, "cache keys unavailable", http.StatusInternalServerError)
				return
			}
		}
		encoded, err := encodeKeysPage(page)
		if err != nil {
			http.Error(w, "cache keys unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}

// parseKeysRequest accepts only a bodyless GET keysPath, optionally followed
// by exactly one after parameter holding a valid continuation token.
func parseKeysRequest(r *http.Request) (cache.KeyPosition, bool) {
	if r.Method != http.MethodGet || r.ContentLength != 0 {
		return cache.KeyPosition{}, false
	}
	if r.RequestURI == keysPath {
		return cache.KeyPosition{}, true
	}
	token, found := strings.CutPrefix(r.RequestURI, keysPath+"?after=")
	if !found {
		return cache.KeyPosition{}, false
	}
	after, err := decodeKeysCursor(token)
	return after, err == nil
}
