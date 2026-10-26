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

func TestRepositoryGrantSurvivesMissingObjectRevalidation(t *testing.T) {
	const seed = "approved seed"
	const layer = "another client's cached layer"
	for _, code := range []int{404, 410} {
		for _, header := range []string{"Cache-Control", "If-None-Match"} {
			t.Run(fmt.Sprintf("%d/%s", code, header), func(t *testing.T) {
				var gets atomic.Int32
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gets.Add(1)
					if r.URL.Path == blobURL("missing") && header == "Cache-Control" {
						http.Redirect(w, r, "/missing-result", http.StatusTemporaryRedirect)
						return
					}
					if r.URL.Path == blobURL("missing") || r.URL.Path == "/missing-result" {
						http.Error(w, "missing", code)
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
				resp, body = getBlob(t, client, origin.URL+blobURL(seed), "Bearer reader")
				if resp.StatusCode != 200 || body != seed {
					t.Fatalf("approval = %d %q", resp.StatusCode, body)
				}
				req := authorizationRequest(t, origin.URL+blobURL("missing"), "Bearer reader")
				value := "no-cache"
				if header == "If-None-Match" {
					value = `"previous"`
				}
				req.Header.Set(header, value)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorizationBody(t, resp, code, "missing\n")
				resp, body = getBlob(t, client, origin.URL+blobURL(layer), "Bearer reader")
				wantGETs := int32(3)
				if header == "Cache-Control" {
					wantGETs++ // Revalidation follows an upstream redirect.
				}
				if resp.StatusCode != 200 || body != layer || gets.Load() != wantGETs {
					t.Fatalf("grant after %d = %d %q, upstream GETs = %d, want %d", code, resp.StatusCode, body, gets.Load(), wantGETs)
				}
			})
		}
	}
}
