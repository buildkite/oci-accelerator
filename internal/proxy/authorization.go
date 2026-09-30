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
// a request. Removing an entry also fences out its outstanding checks. The HMAC
// key is never persisted, so restart clears every grant, and no credentials,
// raw header values, signed redirect URLs, or reversible keys are retained.
type authorizationCache struct {
	mu      sync.Mutex                       // guards all fields below against concurrent requests
	secret  [32]byte                         // per-process HMAC key for fingerprinting requests
	ttl     time.Duration                    // maximum lifetime granted to any cached authorization
	limit   int                              // maximum number of entries before LRU eviction
	entries map[[32]byte]*authorizationEntry // fingerprint to entry lookup for grants and pending checks
	lru     list.List                        // recency ordering of entries, newest at the front
}

type authorizationEntry struct {
	key     [32]byte            // request fingerprint keying this entry in the cache
	element *list.Element       // this entry's node in the LRU list, for O(1) reordering and removal
	grant   *authorizationGrant // cached grant, or nil while a check is pending or after invalidation
}

type authorizationGrant struct {
	meta    cache.Metadata // cached object metadata to serve without re-authorizing upstream
	expires time.Time      // instant past which the grant is stale and must not be reused
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

// commit records a successful authorization on e's entry. It fences successes
// whose entry was invalidated or evicted while the request was in flight: the
// grant is stored only if e is still the current entry for its identity.
func (c *authorizationCache) commit(e *authorizationEntry, grant *authorizationGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries[e.key] == e {
		e.grant = grant
	}
}

// invalidate abandons any cached authorization for e's identity. It removes the
// current entry even if a later attempt replaced e, so a denial or nonreusable
// response cannot be overridden by a concurrent success. A transient failure
// without authentication-state headers leaves the existing grant intact.
func (c *authorizationCache) invalidate(e *authorizationEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if current := c.entries[e.key]; current != nil {
		c.remove(current)
	}
}

func (c *authorizationCache) valid(e *authorizationEntry, g *authorizationGrant) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.entries[e.key] == e && e.grant == g && time.Now().Before(g.expires)
}

// fingerprint hashes the request line and forwarded headers as the HTTP/1.1
// transport sends them, so wire-equivalent GETs share one grant.
func (c *authorizationCache) fingerprint(req *http.Request) [32]byte {
	// Normalize the host to a canonical host:port authority.
	authority, _ := hostport.Parse(req.URL.Host, "443")

	// Merge header names that differ only by case into lowercased buckets,
	// visiting them in sorted order so folding is deterministic.
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

	// The upstream transport is HTTP/1.1-only and receives bodyless GETs.
	// These Header fields are ignored: authority is pinned in Request.Host
	// (and hashed from URL.Host below), and there is no length or transfer data.
	for _, name := range []string{"host", "content-length", "transfer-encoding", "trailer"} {
		delete(headers, name)
	}
	// net/http sends only the first User-Agent value, omitting an explicit
	// empty value. ReverseProxy supplies that empty value when the client has
	// no User-Agent; direct transport callers get the HTTP/1.1 default instead.
	if values, present := headers["user-agent"]; !present {
		headers["user-agent"] = []string{"Go-http-client/1.1"}
	} else if len(values) == 0 || values[0] == "" {
		delete(headers, "user-agent")
	} else {
		headers["user-agent"] = values[:1]
	}

	// Keyed HMAC with a length-prefixed writer so field boundaries are unambiguous.
	mac := hmac.New(sha256.New, c.secret[:])
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		mac.Write(size[:])
		mac.Write([]byte(value))
	}

	// Absorb the request line: method, scheme, authority, path, and query.
	for _, value := range []string{req.Method, req.URL.Scheme, authority.String(), req.URL.EscapedPath(), req.URL.RawQuery} {
		write(value)
	}

	// Absorb the folded headers in sorted order, each with its value count.
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

	// Finalize the digest into a fixed-size cache key.
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

func (c *authorizationCache) remove(e *authorizationEntry) {
	delete(c.entries, e.key)
	c.lru.Remove(e.element)
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
