package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

var manifestMediaTypes = []string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}

func manifestURL(repository, body string) string {
	return fmt.Sprintf("/v2/%s/manifests/sha256:%x", repository, sha256.Sum256([]byte(body)))
}

func manifestHeaders(w http.ResponseWriter, body, mediaType string) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
}

func TestManifestCacheColdWarmAndRestart(t *testing.T) {
	for _, mediaType := range manifestMediaTypes {
		t.Run(mediaType, func(t *testing.T) {
			const body = `{"schemaVersion":2,"marker":"cached"}`
			var calls atomic.Int32
			var revoked atomic.Bool
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if revoked.Load() || r.Header.Get("Authorization") != "Bearer allowed" || r.Header.Get("Accept") != mediaType {
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, "denied")
					return
				}
				manifestHeaders(w, body, mediaType)
				w.Header().Set("X-Origin-Request", fmt.Sprint(n))
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if n == 1 {
					io.WriteString(w, body)
					return
				}
				<-r.Context().Done()
			}))
			defer origin.Close()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			u := origin.URL + manifestURL("team/image", body)
			get := func(client *http.Client, token string) (*http.Response, string) {
				t.Helper()
				r, _ := http.NewRequest(http.MethodGet, u, nil)
				r.Header.Set("Authorization", token)
				r.Header.Set("Accept", mediaType)
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				return resp, string(b)
			}
			for wantRequest := 1; wantRequest <= 2; wantRequest++ {
				resp, got := get(c, "Bearer allowed")
				if got != body || resp.Header.Get("X-Origin-Request") != fmt.Sprint(wantRequest) {
					t.Fatalf("request %d: status=%d body=%q headers=%v", wantRequest, resp.StatusCode, got, resp.Header)
				}
			}

			// Exercise persistence, not merely reuse of an in-memory Store.
			dir := p.cfg.Cache.Dir
			shutdown(t, p)
			if err := p.cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := cache.Open(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			p2, _, c2 := interceptFixture(t, origin, nil, true)
			p2.cache = reopened
			t.Cleanup(func() { _ = reopened.Close() })
			resp, got := get(c2, "Bearer allowed")
			if got != body || resp.Header.Get("X-Origin-Request") != "3" {
				t.Fatalf("restart response: status=%d body=%q headers=%v", resp.StatusCode, got, resp.Header)
			}
			for _, token := range []string{"Bearer denied", ""} {
				resp, got = get(c2, token)
				if resp.StatusCode != http.StatusUnauthorized || got != "denied" {
					t.Fatal("cached manifest bypassed fresh authorization")
				}
			}
			revoked.Store(true)
			resp, got = get(c2, "Bearer allowed")
			if resp.StatusCode != 401 || got != "denied" || calls.Load() != 6 {
				t.Fatal("revoked client received cached manifest or skipped GET")
			}
			origin.Close()
			resp, got = get(c2, "Bearer allowed")
			if resp.StatusCode != http.StatusBadGateway || got == body {
				t.Fatal("upstream failure fell back to cached manifest")
			}
		})
	}
}

func TestManifestWarmMetadataBypasses(t *testing.T) {
	const body = `{"schemaVersion":2}`
	for _, mode := range []string{"missing-type", "type-mismatch", "length-mismatch", "digest-mismatch", "no-store", "unknown-type", "accept-key"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				out, media := body, manifestMediaTypes[0]
				if n > 1 {
					out = strings.Repeat("x", len(body))
					switch mode {
					case "missing-type":
						media = ""
					case "type-mismatch":
						media = manifestMediaTypes[1]
					case "length-mismatch":
						out = "longer origin replacement"
					case "digest-mismatch":
						w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64))
					case "no-store":
						w.Header().Set("Cache-Control", "no-store")
					case "unknown-type":
						media = "application/json"
					}
				}
				if media != "" {
					manifestHeaders(w, out, media)
				} else {
					w.Header()["Content-Type"] = nil // Suppress net/http sniffing.
					w.Header().Set("Content-Length", fmt.Sprint(len(out)))
				}
				io.WriteString(w, out)
			}))
			defer origin.Close()
			_, c := cacheFixture(t, origin, 1<<20, nil)
			u := origin.URL + manifestURL("team/image", body)
			r, _ := http.NewRequest(http.MethodGet, u, nil)
			r.Header.Set("Accept", manifestMediaTypes[0])
			resp, err := c.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			r, _ = http.NewRequest(http.MethodGet, u, nil)
			if mode == "accept-key" {
				r.Header.Set("Accept", manifestMediaTypes[1])
			} else {
				r.Header.Set("Accept", manifestMediaTypes[0])
			}
			resp, err = c.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			want := strings.Repeat("x", len(body))
			if mode == "length-mismatch" {
				want = "longer origin replacement"
			}
			if err != nil || string(got) != want || calls.Load() != 2 {
				t.Fatalf("warm bypass mode=%s body=%q err=%v calls=%d", mode, got, err, calls.Load())
			}
		})
	}
}

func TestManifestBadColdBodyIsNotPublished(t *testing.T) {
	for _, mode := range []string{"digest", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			const expected = `{"schemaVersion":2}`
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				manifestHeaders(w, expected, manifestMediaTypes[0])
				if mode == "digest" {
					io.WriteString(w, strings.Repeat("x", len(expected)))
				} else {
					io.WriteString(w, expected[:3])
				}
			}))
			defer origin.Close()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			resp, err := c.Get(origin.URL + manifestURL("team/image", expected))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			deadline := time.Now().Add(time.Second)
			for p.cache.Stats().Fills != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if stats := p.cache.Stats(); stats.Entries != 0 || stats.Fills != 0 || stats.Reserved != 0 {
				t.Fatalf("bad manifest published: %+v", stats)
			}
		})
	}
}

func TestManifestCacheSeparatesRepositoriesAndRegistries(t *testing.T) {
	const body = `{"schemaVersion":2,"isolation":true}`
	handler := func(w http.ResponseWriter, r *http.Request) {
		manifestHeaders(w, body, manifestMediaTypes[0])
		if strings.Contains(r.URL.Path, "/other/") {
			io.WriteString(w, strings.Repeat("x", len(body)))
		} else {
			io.WriteString(w, body)
		}
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(handler))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	_, _ = getBlob(t, c, origin.URL+manifestURL("team/image", body), "")
	_, got := getBlob(t, c, origin.URL+manifestURL("other/image", body), "")
	if got == body {
		t.Fatal("cross-repository manifest substitution")
	}
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		manifestHeaders(w, body, manifestMediaTypes[0])
		io.WriteString(w, strings.Repeat("y", len(body)))
	}))
	defer other.Close()
	p2, _, c2 := interceptFixture(t, other, nil, true)
	p2.cache = p.cache
	_, got = getBlob(t, c2, other.URL+manifestURL("team/image", body), "")
	if got == body {
		t.Fatal("cross-registry manifest substitution")
	}
}
