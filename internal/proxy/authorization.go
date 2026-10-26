package proxy

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net/http"
	"regexp"
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
// Both request classes share the entry limit. Each approval has a fixed expiry
// selected by header presence, and hits never renew it.
type authorizationCache struct {
	mu               sync.Mutex                       // guards all fields below against concurrent requests
	secret           [32]byte                         // per-process HMAC key for fingerprinting requests
	authorizationTTL time.Duration                    // maximum lifetime for requests carrying Authorization, even empty
	anonymousTTL     time.Duration                    // maximum lifetime for requests without Authorization
	limit            int                              // maximum number of entries before LRU eviction
	entries          map[[32]byte]*authorizationEntry // fingerprint to entry lookup for grants and pending checks
	lru              list.List                        // recency ordering of entries, newest at the front
}

type authorizationEntry struct {
	key     [32]byte            // request fingerprint keying this entry in the cache
	element *list.Element       // this entry's node in the LRU list, for O(1) reordering and removal
	grant   *authorizationGrant // cached grant, or nil while a check is pending or after invalidation
}

type authorizationGrant struct {
	meta    cache.Metadata // representation for exact-request grants, unused by repository grants
	expires time.Time      // instant past which the grant is stale and must not be reused
}

func newAuthorizationCache(authorizationTTL, anonymousTTL time.Duration, limit int) *authorizationCache {
	if (authorizationTTL <= 0 && anonymousTTL <= 0) || limit <= 0 {
		return nil
	}

	c := &authorizationCache{authorizationTTL: authorizationTTL, anonymousTTL: anonymousTTL, limit: limit, entries: make(map[[32]byte]*authorizationEntry)}

	// crypto/rand.Read terminates the process if secure randomness is unavailable.
	rand.Read(c.secret[:])
	return c
}

func (c *authorizationCache) start(req *http.Request) (*authorizationEntry, *authorizationGrant) {
	return c.startKey(c.fingerprint(req))
}

func (c *authorizationCache) startKey(key [32]byte) (*authorizationEntry, *authorizationGrant) {
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

// lookup never allocates an attempt or renews approval. Only upstream checks
// register entries, so cache hits cannot evict another token's pending check.
func (c *authorizationCache) lookup(key [32]byte) (*authorizationEntry, *authorizationGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entries[key]
	if e == nil || e.grant == nil || !time.Now().Before(e.grant.expires) {
		return nil, nil
	}

	c.lru.MoveToFront(e.element)
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
	c.invalidateKey(e.key)
}

func (c *authorizationCache) invalidateKey(key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if current := c.entries[key]; current != nil {
		c.remove(current)
	}
}

func (c *authorizationCache) valid(e *authorizationEntry, g *authorizationGrant) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.entries[e.key] == e && e.grant == g && time.Now().Before(g.expires)
}

// fingerprint scopes bearer pull permission to an exact registry repository.
// Other credentials retain exact-request grants.
func (c *authorizationCache) fingerprint(req *http.Request) [32]byte {
	if object, token, ok := repositoryBearer(req); ok {
		return c.repositoryFingerprint(req, object.repository, token)
	}

	return c.exactFingerprint(req)
}

func (c *authorizationCache) repositoryFingerprint(req *http.Request, repository, token string) [32]byte {
	authority, _ := hostport.Parse(req.URL.Host, "443")
	mac := hmac.New(sha256.New, c.secret[:])
	// Token and repository cannot contain NUL, so boundaries are unambiguous.
	mac.Write([]byte("repository-bearer\x00" + authority.String() + "\x00" + repository + "\x00" + token))

	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

// exactFingerprint identifies a response independently of authorization scope.
func (c *authorizationCache) exactFingerprint(req *http.Request) [32]byte {
	return c.responseFingerprint(req, false)
}

// Shared response identities omit only Authorization and use a separate domain
// so an anonymous exact-request entry can never collide with shared content.
func (c *authorizationCache) responseFingerprint(req *http.Request, shared bool) [32]byte {
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
		if shared && canonical == "authorization" {
			continue
		}

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
	// no User-Agent, while direct transport callers get the HTTP/1.1 default instead.
	if values, present := headers["user-agent"]; !present {
		headers["user-agent"] = []string{"Go-http-client/1.1"}
	} else if len(values) == 0 || values[0] == "" {
		delete(headers, "user-agent")
	} else {
		headers["user-agent"] = values[:1]
	}

	// Keyed HMAC with a length-prefixed writer so field boundaries are unambiguous.
	mac := hmac.New(sha256.New, c.secret[:])
	if shared {
		mac.Write([]byte("shared-mutable\x00"))
	} else {
		mac.Write([]byte("exact-request\x00"))
	}

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

	meta := g.meta
	object, _, repository := repositoryBearer(req)
	if repository {
		var err error
		meta, err = t.cache.Lookup(objectRequestKey(req, object))
		if err != nil {
			return nil
		}

		// Recheck request eligibility and the stored representation's identity,
		// not the object that originally established repository approval.
		expected, ok := objectRequestMetadata(req, meta.MediaType)
		if !ok || expected.Key != meta.Key || expected.Digest != meta.Digest {
			return nil
		}
	}

	body, err := t.cache.Get(req.Context(), meta)
	if err != nil {
		return nil
	}

	// Verification can block. Recheck expiry and invalidation after acquiring
	// the reader. No store lock is held during disk I/O.
	if req.Context().Err() != nil || !t.authorization.valid(e, g) {
		body.Close()
		return nil
	}

	stats.cache = "hit"
	return cachedResponse(req, meta, body)
}

func cachedResponse(req *http.Request, meta cache.Metadata, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Request: req, Body: body, ContentLength: meta.Size,
		Header: http.Header{
			"Content-Type":          {meta.MediaType},
			"Content-Length":        {strconv.FormatInt(meta.Size, 10)},
			"Docker-Content-Digest": {meta.Digest},
			// Do not let downstream caches turn this bounded authorization
			// approval into an independently reusable response.
			"Cache-Control": {"private, no-store"},
			"Date":          {time.Now().UTC().Format(http.TimeFormat)},
		},
	}
}

var bearerCredential = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// repositoryBearer also identifies bypassed immutable reads so their denials
// revoke the same repository grant. It never treats tags or writes as approvals.
func repositoryBearer(req *http.Request) (immutableObject, string, bool) {
	if req.Method != http.MethodGet || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.RawPath != "" {
		return immutableObject{}, "", false
	}

	parts := objectPath.FindStringSubmatch(req.URL.Path)
	token, ok := bearerToken(req)
	if parts == nil || !ok {
		return immutableObject{}, "", false
	}

	kind := blobKind
	if parts[2] == "manifests" {
		kind = manifestKind
	}
	return immutableObject{repository: parts[1], digest: parts[3], kind: kind}, token, true
}

func bearerToken(req *http.Request) (string, bool) {
	values := req.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	scheme, token, ok := strings.Cut(values[0], " ")
	token = strings.TrimLeft(token, " ")
	return token, ok && strings.EqualFold(scheme, "Bearer") && bearerCredential.MatchString(token)
}

// lifetime selects the TTL by header presence, not credential validity. An
// empty Authorization is still credentialed, and zero disables its class.
func (c *authorizationCache) lifetime(h http.Header) time.Duration {
	for name := range h {
		if strings.EqualFold(name, "Authorization") {
			return c.authorizationTTL
		}
	}
	return c.anonymousTTL
}
