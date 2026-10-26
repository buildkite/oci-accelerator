package proxy

import (
	"bytes"
	"container/list"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	mutableMaxResponse = 1 << 20
	mutableMaxBytes    = 32 << 20
	mutableMaxCaptures = 16
	mutableMaxEntries  = 1024
	mutableReservation = mutableMaxResponse + maxHeaderBytes + 256
)

// mutableCache owns response buffers, not credentials. Pending captures reserve
// their maximum allocation before contacting upstream. Completed entries use LRU
// eviction, but an evicted buffer remains charged until its last reader closes.
// All mutable fields are protected by mu. Published headers and bytes are immutable.
type mutableCache struct {
	ttl      time.Duration
	mu       sync.Mutex
	entries  map[[32]byte]*mutableEntry
	lru      list.List
	used     int
	captures int
}

type mutableEntry struct {
	key           [32]byte
	done          chan struct{}
	pending       bool
	element       *list.Element
	header        http.Header
	body          []byte
	contentLength int64
	expires       time.Time
	charge        int
	refs          int
}

func newMutableCache(ttl time.Duration) *mutableCache {
	if ttl <= 0 {
		return nil
	}

	return &mutableCache{ttl: ttl, entries: make(map[[32]byte]*mutableEntry)}
}

// acquire returns a hit, an existing pending entry to wait on, or a new capture.
// A nil entry and response mean admission was refused and delivery must bypass.
func (c *mutableCache) acquire(req *http.Request, key [32]byte) (resp *http.Response, entry *mutableEntry, leader bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.expireLocked()
	if e := c.entries[key]; e != nil {
		if e.pending {
			return nil, e, false
		}

		return c.responseLocked(req, e), nil, false
	}

	if c.captures == mutableMaxCaptures {
		return nil, nil, false
	}

	for (c.used+mutableReservation > mutableMaxBytes || len(c.entries) >= mutableMaxEntries) && c.lru.Len() > 0 {
		c.removeLocked(c.lru.Back().Value.(*mutableEntry))
	}
	if c.used+mutableReservation > mutableMaxBytes || len(c.entries) >= mutableMaxEntries {
		return nil, nil, false
	}

	// The index and the capture each own a reference. Invalidating the index
	// must not release memory still owned by the upstream capture.
	e := &mutableEntry{key: key, done: make(chan struct{}), pending: true, charge: mutableReservation, refs: 2}
	c.entries[key] = e
	c.used += e.charge
	c.captures++
	return nil, e, true
}

func (c *mutableCache) hit(req *http.Request, key [32]byte) *http.Response {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.expireLocked()
	if e := c.entries[key]; e != nil && !e.pending {
		return c.responseLocked(req, e)
	}

	return nil
}

// finish publishes only a complete, still-fresh response. A nil header aborts.
// The capture must relinquish its own buffer references before releasing its reservation.
func (c *mutableCache) finish(e *mutableEntry, header http.Header, body []byte, length int64, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e.pending = false
	c.captures--
	current := c.entries[e.key] == e
	if current && header != nil && time.Now().Before(expires) {
		e.header, e.body, e.contentLength, e.expires = header, body, length, expires
		charge := cap(body) + mutableHeaderSize(header) + 256
		c.used -= e.charge - charge
		e.charge = charge
		e.element = c.lru.PushFront(e)
	} else if current {
		c.removeLocked(e)
	}
	c.releaseLocked(e)
	close(e.done)
}

// An observed denial fences out older successes, including pending captures.
// The capture still owns cleanup and notification when its stream ends.
func (c *mutableCache) invalidate(key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e := c.entries[key]; e != nil {
		c.removeLocked(e)
	}
}

func (c *mutableCache) responseLocked(req *http.Request, e *mutableEntry) *http.Response {
	c.lru.MoveToFront(e.element)
	e.refs++
	header := e.header.Clone()
	header.Set("Cache-Control", "private, no-store")
	if e.contentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(e.contentLength, 10))
	} else {
		header.Del("Content-Length")
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Request: req, Header: header, ContentLength: e.contentLength,
		Body: &mutableReader{Reader: bytes.NewReader(e.body), cache: c, entry: e},
	}
}

func (c *mutableCache) expireLocked() {
	now := time.Now()
	for el := c.lru.Back(); el != nil; {
		previous := el.Prev()
		e := el.Value.(*mutableEntry)
		if !now.Before(e.expires) {
			c.removeLocked(e)
		}
		el = previous
	}
}

func (c *mutableCache) removeLocked(e *mutableEntry) {
	delete(c.entries, e.key)
	if e.element != nil {
		c.lru.Remove(e.element)
	}
	c.releaseLocked(e)
}

func (c *mutableCache) releaseLocked(e *mutableEntry) {
	e.refs--
	if e.refs == 0 {
		c.used -= e.charge
		e.header, e.body = nil, nil
	}
}

// Include field/slice bookkeeping so a response with many tiny headers cannot
// evade the header budget. Response buffers are accounted by capacity, not length.
func mutableHeaderSize(h http.Header) int {
	n := 0
	for name, values := range h {
		n += len(name) + 64
		for _, value := range values {
			n += len(value) + 32
		}
	}
	return n
}

type mutableReader struct {
	*bytes.Reader
	cache *mutableCache
	entry *mutableEntry
}

func (r *mutableReader) Close() error {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()

	if r.entry != nil {
		r.Reader = nil
		r.cache.releaseLocked(r.entry)
		r.entry = nil
	}
	return nil
}
