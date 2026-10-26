package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutableSharedResponsesRequireIndependentRepositoryApproval(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer stranger" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if r.URL.Path == blobURL("seed") {
			blobHeaders(w, "seed")
			io.WriteString(w, "seed")
			return
		}
		body := r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Accept") + " " + r.Header.Get("Cookie")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, client := mutableProxyFixture(t, origin)
	p.cfg.Cache.MutableShared = true

	for _, token := range []string{"a", "b"} {
		path := "/v2/team/image/manifests/seed-a"
		if token == "b" {
			path = blobURL("seed") // Immutable approval must also cover shared metadata.
		}
		resp, _ := getBlob(t, client, origin.URL+path, "Bearer "+token)
		if resp.StatusCode != 200 {
			t.Fatal("seed approval failed")
		}
	}

	for _, path := range []string{
		"/v2/team/image/manifests/latest",
		"/v2/team/image/tags/list?n=2&last=a",
		"/v2/team/image/tags/list?last=a&n=2",
		"/v2/team/image/referrers/" + testBlobDigest + "?artifactType=example",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if method == http.MethodHead && !strings.Contains(path, "/manifests/") {
				continue
			}
			for _, variant := range []http.Header{{}, {"Accept": {"application/example"}}, {"Cookie": {"variant=other"}}} {
				before := calls.Load()
				for _, token := range []string{"a", "b", "a"} {
					req := authorizationRequest(t, origin.URL+path, "Bearer "+token)
					req.Method = method
					for name, values := range variant {
						req.Header[name] = values
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					want := method + " " + path + " " + variant.Get("Accept") + " " + variant.Get("Cookie")
					if method == http.MethodHead {
						want = ""
					}
					assertAuthorizationBody(t, resp, 200, want)
				}
				if got := calls.Load() - before; got != 1 {
					t.Fatalf("%s %s variant=%v: upstream=%d, want 1", method, path, variant, got)
				}
			}
		}
	}

	before := calls.Load()
	resp, body := getBlob(t, client, origin.URL+"/v2/team/image/manifests/latest", "Bearer stranger")
	if resp.StatusCode != 403 || body != "denied\n" || calls.Load() != before+1 {
		t.Fatalf("unapproved token borrowed shared bytes: %d %q", resp.StatusCode, body)
	}

	// A sibling repository and a rotated token each require independent approval.
	for _, target := range []struct{ path, token string }{
		{"/v2/team/image-other/manifests/latest", "Bearer b"},
		{"/v2/team/image/manifests/latest", "Bearer rotated"},
	} {
		before := calls.Load()
		resp, _ := getBlob(t, client, origin.URL+target.path, target.token)
		if resp.StatusCode != 200 || calls.Load() != before+1 {
			t.Fatal("repository or token change borrowed approval")
		}
	}

	// Catalog and non-Bearer entries remain credential-specific. Anonymous
	// requests must not collide with the token-independent shared key.
	for _, path := range []string{"/v2/_catalog", "/v2/team/image/manifests/latest"} {
		credentials := []string{"Basic YQ==", "Basic Yg==", ""}
		if path == "/v2/_catalog" {
			credentials = []string{"Bearer a", "Bearer b", ""}
		}
		for _, credential := range credentials {
			before := calls.Load()
			for range 2 {
				resp, _ := getBlob(t, client, origin.URL+path, credential)
				if resp.StatusCode != 200 {
					t.Fatal("exact response failed")
				}
			}
			if calls.Load() != before+1 {
				t.Fatal("exact response lost credential isolation or caching")
			}
		}
	}
}

func TestMutableSharedApprovalServesImmutableLocalObjects(t *testing.T) {
	const layer = "verified layer"
	for _, shared := range []bool{true, false} {
		for _, seed := range []struct{ method, path string }{
			{"GET", "/v2/team/image/manifests/seed"},
			{"HEAD", "/v2/team/image/manifests/seed"},
			{"GET", "/v2/team/image/tags/list"},
			{"GET", "/v2/team/image/referrers/" + testBlobDigest},
		} {
			t.Run(fmt.Sprintf("shared=%t/%s/%s", shared, seed.method, seed.path), func(t *testing.T) {
				var calls atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path == seed.path {
						io.WriteString(w, "metadata")
						return
					}
					if r.Header.Get("Authorization") != "Bearer filler" {
						http.Error(w, "denied", 403)
						return
					}
					blobHeaders(w, layer)
					io.WriteString(w, layer)
				}))
				defer origin.Close()
				p, client := mutableProxyFixture(t, origin)
				p.cfg.Cache.MutableShared = shared

				resp, body := getBlob(t, client, origin.URL+blobURL(layer), "Bearer filler")
				if resp.StatusCode != 200 || body != layer {
					t.Fatal("failed to fill immutable body")
				}

				req := authorizationRequest(t, origin.URL+seed.path, "Bearer reader")
				req.Method = seed.method
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatal("metadata approval failed")
				}

				resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
				if shared {
					if resp.StatusCode != 200 || body != layer || calls.Load() != 2 {
						t.Fatalf("mutable approval did not cover immutable hit: %d %q, calls=%d", resp.StatusCode, body, calls.Load())
					}
				} else if resp.StatusCode != 403 || calls.Load() != 3 {
					t.Fatal("disabled sharing changed immutable approval")
				}
			})
		}
	}
}

func TestMutableSharedResponseAndGrantHaveIndependentExpiry(t *testing.T) {
	for _, expires := range []string{"grant", "response"} {
		t.Run(expires, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, fmt.Sprint(calls.Add(1)))
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			tr.mutableShared = true
			tr.mutable.ttl = time.Minute
			tr.authorization.authorizationTTL = time.Minute
			if expires == "grant" {
				tr.authorization.authorizationTTL = 500 * time.Millisecond
			} else {
				tr.mutable.ttl = 500 * time.Millisecond
			}

			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer reader")
			if got := readMutable(tr, req); got.err != nil || got.body != "1" {
				t.Fatalf("initial capture: %+v", got)
			}

			time.Sleep(300 * time.Millisecond)
			if got := readMutable(tr, req); got.err != nil || got.body != "1" || calls.Load() != 1 {
				t.Fatalf("early hit: %+v calls=%d", got, calls.Load())
			}

			time.Sleep(300 * time.Millisecond)
			if got := readMutable(tr, req); got.err != nil || got.body != "2" || calls.Load() != 2 {
				t.Fatalf("hit renewed %s expiry: %+v calls=%d", expires, got, calls.Load())
			}

			// Reauthorization does not replace a still-fresh shared response.
			want := "2"
			if expires == "grant" {
				want = "1"
			}
			if got := readMutable(tr, req); got.err != nil || got.body != want || calls.Load() != 2 {
				t.Fatalf("independent %s expiry: %+v calls=%d", expires, got, calls.Load())
			}
		})
	}
}

func TestMutableSharedDenialsRevokeBothCachePaths(t *testing.T) {
	const layer = "cached layer"
	for _, deniedPath := range []string{"/v2/team/image/tags/list?revoke=1", blobURL(layer)} {
		t.Run(deniedPath, func(t *testing.T) {
			var calls atomic.Int32
			var revoked atomic.Bool
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if revoked.Load() && r.Header.Get("Authorization") == "Bearer a" {
					http.Error(w, "denied", 403)
					return
				}
				if r.URL.Path == blobURL(layer) {
					blobHeaders(w, layer)
					io.WriteString(w, layer)
					return
				}
				io.WriteString(w, "metadata")
			}))
			defer origin.Close()
			p, client := mutableProxyFixture(t, origin)
			p.cfg.Cache.MutableShared = true

			for _, token := range []string{"a", "b"} {
				resp, body := getBlob(t, client, origin.URL+blobURL(layer), "Bearer "+token)
				if resp.StatusCode != 200 || body != layer {
					t.Fatal("seed immutable approval failed")
				}
			}
			metadataPath := "/v2/team/image/manifests/latest"
			resp, _ := getBlob(t, client, origin.URL+metadataPath, "Bearer a")
			if resp.StatusCode != 200 {
				t.Fatal("seed metadata failed")
			}

			revoked.Store(true)
			req := authorizationRequest(t, origin.URL+deniedPath, "Bearer a")
			req.Header.Set("Cache-Control", "no-cache")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 403, "denied\n")

			before := calls.Load()
			for _, path := range []string{metadataPath, blobURL(layer)} {
				resp, body := getBlob(t, client, origin.URL+path, "Bearer a")
				if resp.StatusCode != 403 || body != "denied\n" {
					t.Fatal("revoked approval survived in another cache path")
				}

				resp, body = getBlob(t, client, origin.URL+path, "Bearer b")
				want := "metadata"
				if path == blobURL(layer) {
					want = layer
				}
				if resp.StatusCode != 200 || body != want {
					t.Fatal("denial removed another token's shared bytes or approval")
				}
			}
			if calls.Load() != before+2 {
				t.Fatal("independently approved token lost its cache hits")
			}
		})
	}
}

func TestMutableSharedRestrictionsDoNotBorrowApproval(t *testing.T) {
	for _, header := range []http.Header{
		{"Cache-Control": {"max-age=0"}},
		{"Vary": {"Authorization"}},
	} {
		t.Run(fmt.Sprint(header), func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for name, values := range header {
					w.Header()[name] = values
				}
				io.WriteString(w, r.Header.Get("Authorization"))
			}))
			defer origin.Close()
			tr := mutableTransportFixture(t, origin)
			tr.mutableShared = true

			for _, token := range []string{"a", "b", "a"} {
				req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer "+token)
				if got := readMutable(tr, req); got.err != nil || got.body != "Bearer "+token {
					t.Fatalf("restricted response borrowed: %+v", got)
				}
			}
			if calls.Load() != 3 {
				t.Fatal("restricted response established reusable approval")
			}
		})
	}
}

func TestMutableSharedStrippedCeilingBelowResponseTTL(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fmt.Sprint(calls.Add(1)))
	}))
	defer origin.Close()
	p, client := mutableProxyFixture(t, origin)
	p.cfg.Cache.MutableShared = true
	p.mutable.ttl = time.Minute
	p.authorization.authorizationTTL = time.Second

	req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer a")
	resp, err := client.Do(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 200, "1")

	// Equal to the grant ceiling, but tighter than the independent response TTL.
	req.Header.Set("Connection", "Cache-Control")
	req.Header.Set("Cache-Control", "max-age=1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorizationBody(t, resp, 200, "2")
	if calls.Load() != 2 {
		t.Fatal("stripping the request ceiling borrowed a looser response")
	}
}

func TestMutableSharedStrippedAuthorizationCeiling(t *testing.T) {
	const layer = "immutable control"
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			var calls atomic.Int32
			var revoked atomic.Bool
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if revoked.Load() && r.Header.Get("Authorization") == "Bearer reader" {
					http.Error(w, "denied", 403)
					return
				}
				if r.URL.Path == blobURL(layer) {
					blobHeaders(w, layer)
					io.WriteString(w, layer)
					return
				}
				io.WriteString(w, fmt.Sprint(n))
			}))
			defer origin.Close()
			p, client := mutableProxyFixture(t, origin)
			p.cfg.Cache.MutableShared = true
			p.mutable.ttl = 100 * time.Millisecond

			getBlob(t, client, origin.URL+blobURL(layer), "Bearer filler")
			req := authorizationRequest(t, origin.URL+"/v2/team/image/manifests/latest", "Bearer reader")
			if existing {
				resp, err := client.Do(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorizationBody(t, resp, 200, "2")
			}

			before := calls.Load()
			// This ceiling is looser than response TTL but tighter than grant TTL.
			req.Header.Set("Connection", "Cache-Control")
			req.Header.Set("Cache-Control", "max-age=1")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			assertAuthorizationBody(t, resp, 200, fmt.Sprint(before+1))
			if calls.Load() != before+1 {
				t.Fatal("stripped ceiling reused existing repository approval")
			}

			revoked.Store(true)
			time.Sleep(1200 * time.Millisecond)
			resp, body := getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
			if resp.StatusCode != 403 || body != "denied\n" || calls.Load() != before+2 {
				t.Fatal("stripped ceiling extended new approval beyond one second")
			}
		})
	}
}
