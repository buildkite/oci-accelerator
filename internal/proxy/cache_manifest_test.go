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

	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

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
			reopened, err := cachetest.Open(dir, 1<<20)
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

func TestManifestTagsAndAcceptRepresentationsRemainIsolated(t *testing.T) {
	const (
		bodyA = `{"schemaVersion":2,"variant":"A"}`
		bodyB = `{"schemaVersion":2,"variant":"B"}`
	)
	var tagB atomic.Bool
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := bodyA
		media := manifestMediaTypes[0]
		if r.URL.Path == "/v2/team/image/manifests/latest" && tagB.Load() {
			body = bodyB
		}
		if r.Header.Get("Accept") == manifestMediaTypes[1] {
			body, media = bodyB, manifestMediaTypes[1]
		}
		manifestHeaders(w, body, media)
		io.WriteString(w, body)
	}))
	defer origin.Close()
	_, client := cacheFixture(t, origin, 1<<20, nil)
	request := func(path, accept string) string {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, origin.URL+path, nil)
		r.Header.Set("Accept", accept)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Type") != accept || resp.ContentLength != int64(len(got)) {
			t.Fatalf("representation headers = %q/%d body=%q", resp.Header.Get("Content-Type"), resp.ContentLength, got)
		}
		return string(got)
	}

	tag := "/v2/team/image/manifests/latest"
	if got := request(tag, manifestMediaTypes[0]); got != bodyA {
		t.Fatalf("tag A = %q", got)
	}
	tagB.Store(true)
	if got := request(tag, manifestMediaTypes[0]); got != bodyB {
		t.Fatalf("mutated tag = %q", got)
	}
	digestA := manifestURL("team/image", bodyA)
	if got := request(digestA, manifestMediaTypes[0]); got != bodyA {
		t.Fatalf("digest A cold = %q", got)
	}
	if got := request(digestA, manifestMediaTypes[0]); got != bodyA {
		t.Fatalf("digest A warm = %q", got)
	}
	if got := request(manifestURL("team/image", bodyB), manifestMediaTypes[1]); got != bodyB {
		t.Fatalf("index representation = %q", got)
	}
	if got := request(digestA, manifestMediaTypes[0]); got != bodyA {
		t.Fatalf("Accept variant contaminated digest A = %q", got)
	}
	if calls.Load() != 6 {
		t.Fatalf("origin GETs = %d, want 6 per-request authorization GETs", calls.Load())
	}
}

func TestWarmManifestProtocolBypasses(t *testing.T) {
	const (
		cached  = `{"schemaVersion":2,"value":"old"}`
		changed = `{"schemaVersion":2,"value":"new"}`
	)
	tests := []struct {
		name       string
		method     string
		query      string
		request    func(http.Header)
		response   func(http.ResponseWriter)
		wantStatus int
		wantBody   string
		wantHeader string
		wantName   string
		wantValue  string
	}{
		{name: "HEAD", method: http.MethodHead, wantStatus: 200, wantBody: "", wantHeader: "head", wantName: "Content-Length", wantValue: fmt.Sprint(len(changed))},
		{name: "Range", request: func(h http.Header) { h.Set("Range", "bytes=0-3") }, wantStatus: 200, wantBody: changed, wantHeader: "range"},
		{name: "conditional", request: func(h http.Header) { h.Set("If-None-Match", `"old"`) }, wantStatus: 200, wantBody: changed, wantHeader: "conditional"},
		{name: "query", query: "?ns=other", wantStatus: 200, wantBody: changed, wantHeader: "query"},
		{name: "encoding", response: func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "custom") }, wantStatus: 200, wantBody: changed, wantHeader: "encoding", wantName: "Content-Encoding", wantValue: "custom"},
		{name: "Vary", response: func(w http.ResponseWriter) { w.Header().Set("Vary", "Authorization") }, wantStatus: 200, wantBody: changed, wantHeader: "vary", wantName: "Vary", wantValue: "Authorization"},
		{name: "206", response: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(changed)-1, len(changed)))
			w.WriteHeader(http.StatusPartialContent)
		}, wantStatus: 206, wantBody: changed, wantHeader: "partial", wantName: "Content-Range", wantValue: fmt.Sprintf("bytes 0-%d/%d", len(changed)-1, len(changed))},
		{name: "304", response: func(w http.ResponseWriter) { w.Header().Set("ETag", `"new"`); w.WriteHeader(http.StatusNotModified) }, wantStatus: 304, wantBody: "", wantHeader: "not-modified", wantName: "ETag", wantValue: `"new"`},
		{name: "416", response: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(changed)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}, wantStatus: 416, wantBody: changed, wantHeader: "unsatisfied", wantName: "Content-Range", wantValue: fmt.Sprintf("bytes */%d", len(changed))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls, authorized atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("Authorization") == "Bearer allowed" {
					authorized.Add(1)
				} else {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				body := cached
				if n == 2 {
					body = changed
				}
				manifestHeaders(w, body, manifestMediaTypes[0])
				if n == 2 {
					w.Header().Set("X-Protocol", tc.wantHeader)
					if tc.response != nil {
						tc.response(w)
					}
				}
				if r.Method != http.MethodHead && !(n == 2 && tc.wantStatus == http.StatusNotModified) {
					io.WriteString(w, body)
				}
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			u := origin.URL + manifestURL("team/image", cached)
			preload, _ := http.NewRequest(http.MethodGet, u, nil)
			preload.Header.Set("Authorization", "Bearer allowed")
			preload.Header.Set("Accept", manifestMediaTypes[0])
			resp, err := client.Do(preload)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || string(got) != cached {
				t.Fatalf("preload: status=%d body=%q err=%v", resp.StatusCode, got, err)
			}
			coldMeta, ok := objectMetadata(preload, resp)
			if !ok {
				t.Fatal("preload response is not cacheable")
			}

			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			r, _ := http.NewRequest(method, u+tc.query, nil)
			r.Header.Set("Authorization", "Bearer allowed")
			r.Header.Set("Accept", manifestMediaTypes[0])
			if tc.request != nil {
				tc.request(r.Header)
			}
			resp, err = client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != tc.wantStatus || string(got) != tc.wantBody || resp.Header.Get("X-Protocol") != tc.wantHeader || resp.Header.Get(tc.wantName) != tc.wantValue {
				t.Fatalf("response: status=%d body=%q X-Protocol=%q %s=%q err=%v", resp.StatusCode, got, resp.Header.Get("X-Protocol"), tc.wantName, resp.Header.Get(tc.wantName), err)
			}
			if calls.Load() != 2 || authorized.Load() != 2 {
				t.Fatalf("origin calls/authorized = %d/%d, want 2/2", calls.Load(), authorized.Load())
			}
			if stats := p.cache.Stats(); stats.Entries != 1 {
				t.Fatalf("bypass poisoned cache: %+v", stats)
			}
			pinned, err := p.cache.Get(preload.Context(), coldMeta)
			if err != nil {
				t.Fatalf("get original cached manifest: %v", err)
			}
			original, readErr := io.ReadAll(pinned)
			closeErr := pinned.Close()
			if readErr != nil || string(original) != cached || closeErr != nil {
				t.Fatalf("original cached manifest: body=%q read err=%v close err=%v", original, readErr, closeErr)
			}
		})
	}
}

func TestWarmManifestIntermediateNoStoreAndCrossOriginReturnStayBypassed(t *testing.T) {
	const (
		cached  = `{"schemaVersion":2,"value":"old"}`
		changed = `{"schemaVersion":2,"value":"new"}`
	)
	var warm atomic.Bool
	var originCalls, authorized, cdnCalls, returnCalls atomic.Int32
	var origin *httptest.Server
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnCalls.Add(1)
		for _, name := range []string{"Authorization", "Cookie", "X-Registry-Secret"} {
			if r.Header.Get(name) != "" {
				t.Errorf("CDN received %s", name)
			}
		}
		w.Header().Set("Location", origin.URL+"/returned")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer cdn.Close()
	origin = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		if r.URL.Path == "/returned" {
			returnCalls.Add(1)
			for _, name := range []string{"Authorization", "Cookie", "X-Registry-Secret"} {
				if r.Header.Get(name) != "" {
					t.Errorf("cross-origin return restored %s", name)
				}
			}
			manifestHeaders(w, changed, manifestMediaTypes[0])
			w.Header().Set("X-Result", "returned")
			io.WriteString(w, changed)
			return
		}
		if r.Header.Get("Authorization") == "Bearer allowed" {
			authorized.Add(1)
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !warm.Load() {
			manifestHeaders(w, cached, manifestMediaTypes[0])
			io.WriteString(w, cached)
			return
		}
		if r.URL.Path == "/intermediate" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Location", cdn.URL+"/object")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Location", "/intermediate")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
	a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
	p.destinations[a.String()] = struct{}{}
	u := origin.URL + manifestURL("team/image", cached)
	request := func() (*http.Request, *http.Response, string) {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Bearer allowed")
		r.Header.Set("Cookie", "secret=x")
		r.Header.Set("X-Registry-Secret", "secret")
		r.Header.Set("Accept", manifestMediaTypes[0])
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return r, resp, string(body)
	}
	coldRequest, resp, got := request()
	if resp.StatusCode != 200 || got != cached {
		t.Fatalf("preload: status=%d body=%q", resp.StatusCode, got)
	}
	coldMeta, ok := objectMetadata(coldRequest, resp)
	if !ok {
		t.Fatal("preload response is not cacheable")
	}
	warm.Store(true)
	_, resp, got = request()
	if resp.StatusCode != 200 || got != changed || resp.Header.Get("X-Result") != "returned" {
		t.Fatalf("warm bypass: status=%d body=%q headers=%v", resp.StatusCode, got, resp.Header)
	}
	if originCalls.Load() != 4 || authorized.Load() != 3 || cdnCalls.Load() != 1 || returnCalls.Load() != 1 {
		t.Fatalf("calls origin/auth/cdn/return = %d/%d/%d/%d", originCalls.Load(), authorized.Load(), cdnCalls.Load(), returnCalls.Load())
	}
	if stats := p.cache.Stats(); stats.Entries != 1 {
		t.Fatalf("redirect bypass poisoned cache: %+v", stats)
	}
	pinned, err := p.cache.Get(coldRequest.Context(), coldMeta)
	if err != nil {
		t.Fatalf("get original cached manifest: %v", err)
	}
	original, readErr := io.ReadAll(pinned)
	closeErr := pinned.Close()
	if readErr != nil || string(original) != cached || closeErr != nil {
		t.Fatalf("original cached manifest: body=%q read err=%v close err=%v", original, readErr, closeErr)
	}
}
