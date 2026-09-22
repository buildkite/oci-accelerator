package proxy

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

// authorizationCache is process-local. Pending checks share the same bounded
// namespace as grants, so neither credentials nor unbounded tombstones survive
// a request. Removing an entry also fences out its outstanding checks.
type authorizationCache struct {
	mu      sync.Mutex
	secret  [32]byte
	ttl     time.Duration
	limit   int
	entries map[[32]byte]*authorizationEntry
	lru     list.List
}

type authorizationEntry struct {
	key     [32]byte
	element *list.Element
	grant   *authorizationGrant
}

type authorizationGrant struct {
	meta    cache.Metadata
	expires time.Time
}

func newAuthorizationCache(ttl time.Duration, limit int) *authorizationCache {
	if ttl <= 0 || limit <= 0 {
		return nil
	}
	c := &authorizationCache{ttl: ttl, limit: limit, entries: make(map[[32]byte]*authorizationEntry)}
	// crypto/rand.Read terminates the process if secure randomness is unavailable.
	rand.Read(c.secret[:])
	return c
}

func (c *authorizationCache) fingerprint(req *http.Request) [32]byte {
	authority, _ := hostport.Parse(req.URL.Host, "443")
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := make(http.Header, len(names))
	for _, name := range names {
		canonical := strings.ToLower(name)
		// Preserve presence (including an empty value list), value order, and
		// exact values, including non-UTF-8 bytes allowed in HTTP field values.
		headers[canonical] = append(headers[canonical], req.Header[name]...)
	}
	mac := hmac.New(sha256.New, c.secret[:])
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		mac.Write(size[:])
		mac.Write([]byte(value))
	}
	for _, value := range []string{req.Method, req.URL.Scheme, authority.String(), req.URL.EscapedPath(), req.URL.RawQuery} {
		write(value)
	}
	names = names[:0]
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		write(name)
		write(strconv.Itoa(len(headers[name])))
		for _, value := range headers[name] {
			write(value)
		}
	}
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

func (c *authorizationCache) start(req *http.Request) (*authorizationEntry, *authorizationGrant) {
	key := c.fingerprint(req)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		if len(c.entries) == c.limit {
			c.remove(c.lru.Back().Value.(*authorizationEntry))
		}
		e = &authorizationEntry{key: key}
		e.element = c.lru.PushFront(e)
		c.entries[key] = e
	} else {
		c.lru.MoveToFront(e.element)
	}
	if e.grant != nil && !time.Now().Before(e.grant.expires) {
		e.grant = nil
	}
	return e, e.grant
}

func (c *authorizationCache) remove(e *authorizationEntry) {
	delete(c.entries, e.key)
	c.lru.Remove(e.element)
}

// complete fences successes whose entry was invalidated or evicted while the
// request was in flight. Denials invalidate even a replacement entry for this
// identity. A transient failure does not call complete at all.
func (c *authorizationCache) complete(e *authorizationEntry, grant *authorizationGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if grant == nil {
		if current := c.entries[e.key]; current != nil {
			c.remove(current)
		}
	} else if c.entries[e.key] == e {
		e.grant = grant
	}
}

func (c *authorizationCache) valid(e *authorizationEntry, g *authorizationGrant) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[e.key] == e && e.grant == g && time.Now().Before(g.expires)
}

func (t registryTransport) authorizedResponse(req *http.Request, e *authorizationEntry, g *authorizationGrant, stats *requestStats) *http.Response {
	if g == nil {
		return nil
	}
	body, err := t.cache.Get(req.Context(), g.meta)
	if err != nil {
		return nil
	}
	// Verification can block. Recheck expiry and invalidation after acquiring
	// the reader; no store lock is held during disk I/O.
	if req.Context().Err() != nil || !t.authorization.valid(e, g) {
		body.Close()
		return nil
	}
	stats.cache = "hit"
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Request: req, Body: body, ContentLength: g.meta.Size,
		Header: http.Header{
			"Content-Type":          {g.meta.MediaType},
			"Content-Length":        {strconv.FormatInt(g.meta.Size, 10)},
			"Docker-Content-Digest": {g.meta.Digest},
			// Do not let downstream caches turn this bounded, request-specific
			// approval into an independently reusable response.
			"Cache-Control": {"private, no-store"},
			"Date":          {time.Now().UTC().Format(http.TimeFormat)},
		},
	}
}
