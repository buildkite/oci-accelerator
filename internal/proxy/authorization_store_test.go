package proxy

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func authorizationRequest(t *testing.T, rawURL, authorization string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header["Authorization"] = []string{authorization}
	}
	return req
}

func TestAuthorizationFingerprintContract(t *testing.T) {
	c := newAuthorizationCache(time.Second, 10)
	base := authorizationRequest(t, "https://REGISTRY.test:443/v2/a%2Fb?x=1", "Bearer token")
	base.Header["X-Multi"] = []string{"first", "second"}
	base.Header["X-Empty"] = []string{}

	canonical := authorizationRequest(t, "https://registry.test/v2/a%2Fb?x=1", "Bearer token")
	canonical.Header["x-multi"] = []string{"first", "second"}
	canonical.Header["x-empty"] = []string{}
	if c.fingerprint(base) != c.fingerprint(canonical) {
		t.Fatal("canonical-equivalent URL or header names produced different fingerprints")
	}

	mutations := map[string]func(*http.Request){
		"authorization absent": func(r *http.Request) { delete(r.Header, "Authorization") },
		"authorization empty":  func(r *http.Request) { r.Header["Authorization"] = []string{""} },
		"value order":          func(r *http.Request) { r.Header["X-Multi"] = []string{"second", "first"} },
		"raw path":             func(r *http.Request) { r.URL.RawPath = "/v2/a/b"; r.URL.Path = "/v2/a/b" },
		"query":                func(r *http.Request) { r.URL.RawQuery = "x=2" },
		"method":               func(r *http.Request) { r.Method = http.MethodHead },
	}
	want := c.fingerprint(base)
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := base.Clone(base.Context())
			r.Header = base.Header.Clone()
			mutate(r)
			if c.fingerprint(r) == want {
				t.Fatal("mutation did not change fingerprint")
			}
		})
	}

	a := authorizationRequest(t, "https://registry.test/v2/x", "")
	b := a.Clone(a.Context())
	a.Header["X-Raw"] = []string{string([]byte{0xff})}
	b.Header["X-Raw"] = []string{string([]byte{0xfe})}
	if c.fingerprint(a) == c.fingerprint(b) {
		t.Fatal("distinct non-UTF-8 header bytes collided")
	}
	other := newAuthorizationCache(time.Second, 10)
	if c.fingerprint(base) == other.fingerprint(base) {
		t.Fatal("process-local secrets did not separate fingerprints")
	}
}

func TestAuthorizationStoreExpiryDoesNotRenewOnHit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newAuthorizationCache(30*time.Second, 2)
		r := authorizationRequest(t, "https://registry.test/v2/x", "Bearer x")
		e, _ := c.start(r)
		g := &authorizationGrant{meta: cache.Metadata{Key: "x"}, expires: time.Now().Add(c.ttl)}
		c.complete(e, g)
		time.Sleep(20 * time.Second)
		if gotEntry, got := c.start(r); gotEntry != e || got != g {
			t.Fatal("grant expired too early")
		}
		time.Sleep(11 * time.Second)
		if gotEntry, got := c.start(r); gotEntry != e || got != nil {
			t.Fatal("cache hit renewed grant expiry")
		}
	})
}

func TestAuthorizationStoreBoundsPendingAndFencesCompletion(t *testing.T) {
	c := newAuthorizationCache(time.Minute, 2)
	r1 := authorizationRequest(t, "https://registry.test/v2/1", "Bearer 1")
	r2 := authorizationRequest(t, "https://registry.test/v2/2", "Bearer 2")
	r3 := authorizationRequest(t, "https://registry.test/v2/3", "Bearer 3")
	c.start(r1)
	e2, _ := c.start(r2)
	c.start(r1) // r1 is most recently used, so pending r2 must be evicted.
	c.start(r3)
	c.complete(e2, &authorizationGrant{expires: time.Now().Add(time.Minute)})
	if _, got := c.start(r2); got != nil {
		t.Fatal("evicted pending completion restored a grant")
	}

	old, _ := c.start(r1)
	c.complete(old, nil)
	replacement, _ := c.start(r1)
	grant := &authorizationGrant{expires: time.Now().Add(time.Minute)}
	c.complete(old, grant)
	if replacement == old {
		t.Fatal("invalidation did not create a new entry")
	}
	if _, got := c.start(r1); got != nil {
		t.Fatal("old pending success restored grant after invalidation")
	}
	if len(c.entries) > c.limit {
		t.Fatalf("entries = %d, limit = %d", len(c.entries), c.limit)
	}
}

func TestAuthorizationDenialAfterEvictionInvalidatesReplacement(t *testing.T) {
	c := newAuthorizationCache(time.Minute, 1)
	req := authorizationRequest(t, "https://registry.test/v2/a", "Bearer a")
	old, _ := c.start(req)
	c.start(authorizationRequest(t, "https://registry.test/v2/b", "Bearer b"))
	replacement, _ := c.start(req)
	c.complete(replacement, &authorizationGrant{expires: time.Now().Add(time.Minute)})
	// The older check's denial is still authoritative even though pressure
	// evicted its bookkeeping entry before the replacement was approved.
	c.complete(old, nil)
	if _, grant := c.start(req); grant != nil {
		t.Fatal("eviction hid a denial from the replacement grant")
	}
	c.complete(replacement, &authorizationGrant{expires: time.Now().Add(time.Minute)})
	if _, grant := c.start(req); grant != nil {
		t.Fatal("invalidation allowed a late replacement approval")
	}
}

func TestAuthorizationStoreConcurrentSafety(t *testing.T) {
	c := newAuthorizationCache(time.Minute, 8)
	var wg sync.WaitGroup
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r := authorizationRequest(t, fmt.Sprintf("https://registry.test/v2/%d", i%16), fmt.Sprintf("Bearer %d", worker%4))
				e, g := c.start(r)
				if g == nil {
					c.complete(e, &authorizationGrant{expires: time.Now().Add(time.Minute)})
				} else {
					c.valid(e, g)
				}
				if i%11 == 0 {
					c.complete(e, nil)
				}
			}
		}(worker)
	}
	wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > c.limit || c.lru.Len() != len(c.entries) {
		t.Fatalf("inconsistent bounded store: entries=%d lru=%d limit=%d", len(c.entries), c.lru.Len(), c.limit)
	}
}
