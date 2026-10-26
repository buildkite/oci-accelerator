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

func TestRepositoryGrantRequestCeiling(t *testing.T) {
	const layer = "0123456789abcdef"
	for _, ranged := range []bool{false, true} {
		for _, ceiling := range []struct {
			value    string
			upstream bool
		}{
			{"", false},
			{"max-age=60", false},
			{"max-age=30", false},
			{"max-age=1", true},
			{"s-maxage=1", true},
		} {
			t.Run(fmt.Sprintf("range=%t/%s", ranged, ceiling.value), func(t *testing.T) {
				var gets atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gets.Add(1)
					blobHeaders(w, layer)
					io.WriteString(w, layer)
				}))
				defer origin.Close()
				p, client := cacheFixture(t, origin, 1<<20, nil)
				p.authorization = newAuthorizationCache(30*time.Second, time.Minute, 100)
				resp, body := getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
				if resp.StatusCode != 200 || body != layer {
					t.Fatalf("approval = %d %q", resp.StatusCode, body)
				}
				// Repeat to cover both an unrestricted grant and a fresh grant
				// established by a request with a tighter ceiling.
				for i := int32(1); i <= 2; i++ {
					req := authorizationRequest(t, origin.URL+blobURL(layer), "Bearer reader")
					if ceiling.value != "" {
						req.Header.Set("Cache-Control", ceiling.value)
					}
					status, want := http.StatusOK, layer
					if ranged {
						req.Header.Set("Range", "bytes=3-8")
						status, want = http.StatusPartialContent, "345678"
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					assertAuthorizationBody(t, resp, status, want)
					wantGETs := int32(1)
					if ceiling.upstream {
						wantGETs += i
					}
					if gets.Load() != wantGETs {
						t.Fatalf("upstream GETs = %d, want %d", gets.Load(), wantGETs)
					}
				}
			})
		}
	}
}

func TestRepositoryGrantReadsOtherClientsObjects(t *testing.T) {
	const seed = "repository approval"
	const layer = "0123456789abcdef"
	const manifest = `{"schemaVersion":2,"layers":[]}`
	for _, rangedApproval := range []bool{false, true} {
		name := "full approval"
		if rangedApproval {
			name = "range approval"
		}
		t.Run(name, func(t *testing.T) {
			objects := map[string]string{
				blobURL(seed):                       seed,
				blobURL(layer):                      layer,
				manifestURL("team/image", manifest): manifest,
				strings.Replace(blobURL(layer), "team/image", "team/image-other", 1): layer,
			}
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				// Only the filler can download other objects. A reader's own
				// approval must therefore come from the seed, not from those GETs.
				if r.Header.Get("Authorization") != "Bearer filler" &&
					(r.Header.Get("Authorization") != "Bearer reader" || r.URL.Path != blobURL(seed)) {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				body, ok := objects[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				blobHeaders(w, body)
				if strings.Contains(r.URL.Path, "/manifests/") {
					w.Header().Set("Content-Type", manifestMediaTypes[0])
				}
				io.WriteString(w, body)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			p.authorization = newAuthorizationCache(time.Minute, time.Minute, 100)
			for path, want := range objects {
				resp, body := getBlob(t, client, origin.URL+path, "Bearer filler")
				if resp.StatusCode != 200 || body != want {
					t.Fatalf("warm %s = %d %q", path, resp.StatusCode, body)
				}
			}
			before := gets.Load()
			if rangedApproval {
				resp, body := requestRange(t, client, origin.URL+blobURL(seed), "Bearer reader", "bytes=2-5")
				if resp.StatusCode != 206 || body != "posi" {
					t.Fatalf("range approval = %d %q", resp.StatusCode, body)
				}
			} else {
				resp, body := getBlob(t, client, origin.URL+blobURL(seed), "Bearer reader")
				if resp.StatusCode != 200 || body != seed {
					t.Fatalf("approval = %d %q", resp.StatusCode, body)
				}
			}
			for _, path := range []string{blobURL(layer), manifestURL("team/image", manifest)} {
				resp, body := getBlob(t, client, origin.URL+path, "Bearer reader")
				if resp.StatusCode != 200 || body != objects[path] || resp.Header.Get("Cache-Control") != "private, no-store" {
					t.Fatalf("repository hit = %d %q %v", resp.StatusCode, body, resp.Header)
				}
			}
			resp, body := requestRange(t, client, origin.URL+blobURL(layer), "Bearer reader", "bytes=3-8")
			if resp.StatusCode != 206 || body != "345678" || gets.Load() != before+1 {
				t.Fatalf("range hit = %d %q, upstream GETs = %d, want %d", resp.StatusCode, body, gets.Load(), before+1)
			}
			// Neither a rotated token nor a sibling repository can borrow approval.
			resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer rotated")
			if resp.StatusCode != 403 || body != "denied\n" {
				t.Fatalf("rotated token = %d %q", resp.StatusCode, body)
			}
			sibling := strings.Replace(blobURL(layer), "team/image", "team/image-other", 1)
			resp, body = getBlob(t, client, origin.URL+sibling, "Bearer reader")
			if resp.StatusCode != 403 || body != "denied\n" {
				t.Fatalf("sibling repository = %d %q", resp.StatusCode, body)
			}
			// A denial on a different digest in the approved repository revokes
			// access to all its cached objects, not just the missing digest.
			resp, body = getBlob(t, client, origin.URL+blobURL("not cached"), "Bearer reader")
			if resp.StatusCode != 403 || body != "denied\n" {
				t.Fatalf("uncached denial = %d %q", resp.StatusCode, body)
			}
			resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
			if resp.StatusCode != 403 || body != "denied\n" || gets.Load() != before+5 {
				t.Fatalf("revoked repository = %d %q, upstream GETs = %d, want %d", resp.StatusCode, body, gets.Load(), before+5)
			}
		})
	}
}

func TestRepositoryGrantRevalidation(t *testing.T) {
	const seed = "approved seed"
	const layer = "another client's cached layer"
	for _, tc := range []struct {
		name, header, value string
		status              int
		restrict            bool
		redirect            bool
		expire              bool
		cold                bool
	}{
		{name: "conditional unchanged", header: "If-None-Match", value: `"previous"`, status: 304},
		{name: "conditional changed", header: "If-None-Match", value: `"previous"`, status: 200},
		{name: "no-cache", header: "Cache-Control", value: "no-cache", status: 200, redirect: true, cold: true},
		{name: "pragma", header: "Pragma", value: "no-cache", status: 200},
		{name: "restricted conditional", header: "If-None-Match", value: `"previous"`, status: 304, restrict: true},
		{name: "restricted response", header: "Cache-Control", value: "no-cache", status: 200, restrict: true},
		{name: "restricted redirect", header: "Cache-Control", value: "no-cache", status: 200, restrict: true, redirect: true},
		{name: "missing conditional", header: "If-None-Match", value: `"previous"`, status: 404},
		{name: "gone conditional", header: "If-None-Match", value: `"previous"`, status: 410},
		{name: "missing redirect", header: "Cache-Control", value: "no-cache", status: 404, redirect: true},
		{name: "gone redirect", header: "Cache-Control", value: "no-cache", status: 410, redirect: true},
		{name: "unauthorized", header: "If-None-Match", value: `"previous"`, status: 401},
		{name: "forbidden", header: "Cache-Control", value: "no-cache", status: 403},
		{name: "original expiry", header: "If-None-Match", value: `"previous"`, status: 200, expire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				if r.URL.Path == blobURL("revalidate") || r.URL.Path == "/revalidation-result" {
					if tc.restrict && r.URL.Path == blobURL("revalidate") {
						w.Header().Set("Cache-Control", "no-store")
					}
					if tc.redirect && r.URL.Path == blobURL("revalidate") {
						http.Redirect(w, r, "/revalidation-result", http.StatusTemporaryRedirect)
						return
					}
					if tc.status == http.StatusNotModified {
						w.WriteHeader(tc.status)
						return
					}
					if tc.status != http.StatusOK {
						http.Error(w, "upstream result", tc.status)
						return
					}
					blobHeaders(w, "revalidate")
					io.WriteString(w, "revalidate")
					return
				}
				body := seed
				if r.URL.Path == blobURL(layer) {
					if r.Header.Get("Authorization") != "Bearer filler" {
						http.Error(w, "denied", 403)
						return
					}
					body = layer
				}
				if tc.expire && r.URL.Path == blobURL(seed) {
					w.Header().Set("Cache-Control", "max-age=1")
				}
				blobHeaders(w, body)
				io.WriteString(w, body)
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			p.authorization = newAuthorizationCache(time.Minute, time.Minute, 100)
			resp, body := getBlob(t, client, origin.URL+blobURL(layer), "Bearer filler")
			if resp.StatusCode != 200 || body != layer {
				t.Fatalf("warm layer = %d %q", resp.StatusCode, body)
			}
			req := authorizationRequest(t, origin.URL+blobURL("revalidate"), "Bearer reader")
			req.Header.Set(tc.header, tc.value)
			if tc.cold {
				// A successful request requiring revalidation cannot create
				// approval for a token that has no repository grant yet.
				resp, err := client.Do(req.Clone(req.Context()))
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorizationBody(t, resp, 200, "revalidate")
				resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
				if resp.StatusCode != 403 || body != "denied\n" || gets.Load() != 4 {
					t.Fatalf("cold reader = %d %q, upstream GETs = %d, want 403 and 4", resp.StatusCode, body, gets.Load())
				}
			}
			resp, body = getBlob(t, client, origin.URL+blobURL(seed), "Bearer reader")
			if resp.StatusCode != 200 || body != seed {
				t.Fatalf("approval = %d %q", resp.StatusCode, body)
			}
			before := gets.Load()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			want := "revalidate"
			if tc.status == http.StatusNotModified {
				want = ""
			} else if tc.status != http.StatusOK {
				want = "upstream result\n"
			}
			assertAuthorizationBody(t, resp, tc.status, want)
			if tc.expire {
				// The original approval had a one-second ceiling. Revalidation
				// must not replace it with the configured one-minute lease.
				time.Sleep(1100 * time.Millisecond)
			}
			resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
			status, want, count := 200, layer, before+1
			if tc.restrict || tc.expire || tc.status == 401 || tc.status == 403 {
				status, want, count = 403, "denied\n", count+1
			}
			if tc.redirect {
				count++
			}
			if resp.StatusCode != status || body != want || gets.Load() != count {
				t.Fatalf("after revalidation = %d %q, upstream GETs = %d, want %d %q and %d", resp.StatusCode, body, gets.Load(), status, want, count)
			}
		})
	}
}
