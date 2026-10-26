package proxy

import (
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestMutableCacheExpiresBeforeEvictingFreshLRU(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMutableCache(time.Hour)
		req := &http.Request{Method: http.MethodHead}
		for i := range 1024 {
			expires := time.Now().Add(time.Hour)
			if i == 1 {
				expires = time.Now().Add(time.Second)
			}
			publishMutableCache(t, cache, req, i, nil, expires)
		}

		// Put the expiring entry ahead of the oldest fresh entry in recency.
		readMutableCache(t, cache, req, 1, "")
		time.Sleep(time.Second)
		publishMutableCache(t, cache, req, 1024, nil, time.Now().Add(time.Hour))
		readMutableCache(t, cache, req, 0, "")
		if resp := cache.hit(req, mutableTestKey(1)); resp != nil {
			resp.Body.Close()
			t.Fatal("expired entry remained a hit")
		}
	})
}

func TestMutableCacheHitsChangeRecencyNotExpiryAndBoundTinyHEADs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMutableCache(time.Hour)
		req := &http.Request{Method: http.MethodHead}
		expires := time.Now().Add(time.Minute)
		for i := range 1024 {
			publishMutableCache(t, cache, req, i, nil, expires)
		}
		time.Sleep(30 * time.Second)
		readMutableCache(t, cache, req, 0, "")
		publishMutableCache(t, cache, req, 1024, nil, time.Now().Add(time.Hour))
		if len(cache.entries) != 1024 {
			t.Fatalf("tiny HEAD entries = %d, want bounded at 1024", len(cache.entries))
		}
		if resp := cache.hit(req, mutableTestKey(1)); resp != nil {
			resp.Body.Close()
			t.Fatal("insertion did not evict the least recently used entry")
		}
		readMutableCache(t, cache, req, 0, "")

		time.Sleep(30 * time.Second)
		if resp := cache.hit(req, mutableTestKey(0)); resp != nil {
			resp.Body.Close()
			t.Fatal("cache hits extended the original expiry")
		}
		readMutableCache(t, cache, req, 1024, "")
	})
}

func TestMutableCachePendingCapturesAreNotEvictionVictims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMutableCache(time.Second)
		req := &http.Request{Method: http.MethodGet}
		var pending []*mutableEntry
		for i := range 16 {
			resp, entry, leader := cache.acquire(req, mutableTestKey(i))
			if resp != nil || entry == nil || !leader {
				t.Fatalf("capture %d was not admitted", i)
			}
			pending = append(pending, entry)
		}

		time.Sleep(2 * time.Second)
		if resp, entry, leader := cache.acquire(req, mutableTestKey(16)); resp != nil || entry != nil || leader {
			t.Fatal("seventeenth simultaneous capture was admitted")
		}
		for i, want := range pending {
			resp, entry, leader := cache.acquire(req, mutableTestKey(i))
			if resp != nil || entry != want || leader {
				t.Fatalf("pending capture %d was replaced or expired", i)
			}
			select {
			case <-entry.done:
				t.Fatalf("pending capture %d was prematurely completed", i)
			default:
			}
		}

		cache.finish(pending[0], nil, nil, 0, time.Time{})
		select {
		case <-pending[0].done:
		default:
			t.Fatal("aborted capture did not release its waiters")
		}
		resp, replacement, leader := cache.acquire(req, mutableTestKey(16))
		if resp != nil || replacement == nil || !leader {
			t.Fatal("aborting a capture did not restore admission")
		}
		cache.finish(replacement, nil, nil, 0, time.Time{})
		for _, entry := range pending[1:] {
			cache.finish(entry, nil, nil, 0, time.Time{})
		}
		if cache.used != 0 {
			t.Fatalf("aborted captures retain %d bytes", cache.used)
		}
	})
}

func TestMutableCacheEvictedReadersPinMemoryWithinReservationBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 32 << 20
		cache := newMutableCache(time.Hour)
		req := &http.Request{Method: http.MethodGet}
		var readers []io.ReadCloser
		var extra io.ReadCloser
		var firstCharge, reservation int
		for i := 0; ; i++ {
			if i == 32 {
				t.Fatal("admitted 32 pinned one-MiB buffers without reserving header space")
			}
			before := cache.used
			resp, entry, leader := cache.acquire(req, mutableTestKey(i))
			if resp != nil {
				resp.Body.Close()
				t.Fatal("unique key unexpectedly hit")
			}
			if entry == nil {
				if leader || len(readers) < 16 {
					t.Fatal("admission stopped before completed reader buffers exhausted the byte budget")
				}
				if cache.used+reservation <= budget {
					t.Fatal("admission refused despite space for a full capture reservation")
				}
				break
			}
			if !leader {
				t.Fatal("unique key joined an existing capture")
			}
			if i == 0 {
				reservation = cache.used - before
			}
			if cache.used > budget {
				t.Fatalf("capture reservation exceeded 32 MiB: %d bytes", cache.used)
			}
			// A one-byte response still owns the full backing allocation.
			body := make([]byte, 1, 1<<20)
			body[0] = byte(i)
			header := http.Header{"X-Metadata": {strings.Repeat("x", 63<<10)}}
			cache.finish(entry, header, body, 1, time.Now().Add(time.Hour))
			if cache.used > budget {
				t.Fatalf("published body and headers exceeded 32 MiB: %d bytes", cache.used)
			}
			resp = cache.hit(req, mutableTestKey(i))
			if resp == nil {
				t.Fatal("completed response was not published")
			}
			readers = append(readers, resp.Body)
			if i == 0 {
				firstCharge = cache.used
				resp = cache.hit(req, mutableTestKey(i))
				if resp == nil {
					t.Fatal("second reader could not acquire the response")
				}
				extra = resp.Body
			}
		}
		if len(cache.entries) != 0 {
			t.Fatal("LRU eviction did not remove indexed entries under byte pressure")
		}
		if cache.used < len(readers)*(1<<20) || cache.used > budget {
			t.Fatalf("pinned allocation accounting = %d for %d one-MiB buffers", cache.used, len(readers))
		}

		before := cache.used
		body, err := io.ReadAll(readers[0])
		if err != nil || string(body) != "\x00" {
			t.Fatalf("evicted reader returned %q, %v", body, err)
		}
		if cache.used != before {
			t.Fatal("EOF released memory before Close")
		}
		if err := readers[0].Close(); err != nil {
			t.Fatal(err)
		}
		if cache.used != before {
			t.Fatal("closing one reader released memory still owned by another")
		}
		body, err = io.ReadAll(extra)
		if err != nil || string(body) != "\x00" {
			t.Fatalf("last evicted reader returned %q, %v", body, err)
		}
		if err := extra.Close(); err != nil {
			t.Fatal(err)
		}
		if cache.used != before-firstCharge {
			t.Fatalf("last Close left %d charged bytes, want %d", cache.used, before-firstCharge)
		}
		if err := extra.Close(); err != nil {
			t.Fatal(err)
		}
		if cache.used != before-firstCharge {
			t.Fatal("repeated Close released memory twice")
		}
		resp, replacement, leader := cache.acquire(req, mutableTestKey(1000))
		if resp != nil || replacement == nil || !leader {
			t.Fatal("closing the last reader did not restore capture admission")
		}
		cache.finish(replacement, nil, nil, 0, time.Time{})
		for i, reader := range readers[1:] {
			body, err := io.ReadAll(reader)
			if err != nil || len(body) != 1 || body[0] != byte(i+1) {
				t.Fatalf("evicted reader %d returned %q, %v", i+1, body, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if cache.used != 0 {
			t.Fatalf("closed readers retain %d charged bytes", cache.used)
		}
	})
}

func TestMutableCacheInvalidatedCaptureCannotPublishOverReplacement(t *testing.T) {
	cache := newMutableCache(time.Hour)
	req := &http.Request{Method: http.MethodGet}
	key := mutableTestKey(0)
	resp, old, leader := cache.acquire(req, key)
	if resp != nil || old == nil || !leader {
		t.Fatal("initial capture was not admitted")
	}
	reservation := cache.used
	cache.invalidate(key)
	if cache.used != reservation {
		t.Fatal("invalidation released the unfinished capture's reservation")
	}
	select {
	case <-old.done:
		t.Fatal("invalidation completed an unfinished capture")
	default:
	}
	resp, replacement, leader := cache.acquire(req, key)
	if resp != nil || replacement == nil || replacement == old || !leader {
		t.Fatal("invalidation did not permit a distinct replacement capture")
	}
	if cache.used != 2*reservation {
		t.Fatal("old and replacement captures did not retain separate reservations")
	}

	var others []*mutableEntry
	for i := 1; i <= 14; i++ {
		resp, entry, leader := cache.acquire(req, mutableTestKey(i))
		if resp != nil || entry == nil || !leader {
			t.Fatalf("capture %d was not admitted", i)
		}
		others = append(others, entry)
	}
	if resp, entry, leader := cache.acquire(req, mutableTestKey(15)); resp != nil || entry != nil || leader {
		t.Fatal("invalidation released the unfinished capture's slot")
	}

	cache.finish(old, http.Header{}, []byte("stale"), 5, time.Now().Add(time.Hour))
	select {
	case <-old.done:
	default:
		t.Fatal("finishing the invalidated capture did not release its waiters")
	}
	if cache.used != 15*reservation {
		t.Fatal("finishing the invalidated capture did not release exactly its reservation")
	}
	if resp := cache.hit(req, key); resp != nil {
		resp.Body.Close()
		t.Fatal("invalidated capture published stale bytes")
	}
	if resp, entry, leader := cache.acquire(req, key); resp != nil || entry != replacement || leader {
		t.Fatal("old finish deleted or replaced the pending replacement")
	}
	resp, entry, leader := cache.acquire(req, mutableTestKey(15))
	if resp != nil || entry == nil || !leader {
		t.Fatal("finishing the invalidated capture did not restore its capture slot")
	}
	others = append(others, entry)
	for _, entry := range others {
		cache.finish(entry, nil, nil, 0, time.Time{})
	}
	cache.finish(replacement, http.Header{}, []byte("replacement"), 11, time.Now().Add(time.Hour))
	readMutableCache(t, cache, req, 0, "replacement")
	cache.invalidate(key)
	if cache.used != 0 || cache.captures != 0 || len(cache.entries) != 0 {
		t.Fatalf("finished captures retain bytes=%d captures=%d entries=%d", cache.used, cache.captures, len(cache.entries))
	}
}

func mutableTestKey(i int) (key [32]byte) {
	binary.LittleEndian.PutUint64(key[:], uint64(i))
	return key
}

func publishMutableCache(t *testing.T, cache *mutableCache, req *http.Request, key int, body []byte, expires time.Time) {
	t.Helper()
	resp, entry, leader := cache.acquire(req, mutableTestKey(key))
	if resp != nil || entry == nil || !leader {
		t.Fatalf("key %d was not admitted for capture", key)
	}
	cache.finish(entry, http.Header{}, body, int64(len(body)), expires)
}

func readMutableCache(t *testing.T, cache *mutableCache, req *http.Request, key int, want string) {
	t.Helper()
	resp := cache.hit(req, mutableTestKey(key))
	if resp == nil {
		t.Fatalf("key %d was not a cache hit", key)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != want {
		t.Fatalf("key %d returned %q, %v, want %q", key, body, err, want)
	}
}
