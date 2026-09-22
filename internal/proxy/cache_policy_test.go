package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

func TestBlobMetadataAcceptsTerminalBlobResponse(t *testing.T) {
	req, resp := cachePolicyFixture()
	resp.Header.Set("Docker-Content-Digest", testBlobDigest)
	meta, ok := objectMetadata(req, resp)
	if !ok {
		t.Fatal("matching terminal blob response was rejected")
	}
	if meta.Digest != testBlobDigest || meta.Size != 3 || meta.MediaType != "application/octet-stream" || meta.Key == "" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	resp.Header.Del("Docker-Content-Digest")
	if _, ok := objectMetadata(req, resp); !ok {
		t.Fatal("response without optional CDN digest was rejected")
	}
	if meta.Key != "ce77ff66e6785141501fc0f3fd7fbedac4bb24e00a106e0830aecff478edf8f3" {
		t.Fatalf("legacy blob key changed: %s", meta.Key)
	}
}

func TestBlobMetadataRejectsUnsupportedRequests(t *testing.T) {
	tests := map[string]func(*http.Request){
		"tag manifest":       func(r *http.Request) { r.URL.Path = "/v2/team/image/manifests/latest" },
		"HEAD":               func(r *http.Request) { r.Method = http.MethodHead },
		"query":              func(r *http.Request) { r.URL.RawQuery = "ns=other" },
		"empty query marker": func(r *http.Request) { r.URL.ForceQuery = true },
		"range":              func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") },
		"conditional":        func(r *http.Request) { r.Header.Set("If-None-Match", `"etag"`) },
		"unsupported digest": func(r *http.Request) { r.URL.Path = "/v2/team/image/blobs/sha512:" + strings.Repeat("a", 128) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			req, resp := cachePolicyFixture()
			change(req)
			if _, ok := objectMetadata(req, resp); ok {
				t.Fatal("unsupported request was cacheable")
			}
		})
	}
}

func TestManifestMetadataAdmission(t *testing.T) {
	for _, mediaType := range manifestMediaTypes {
		t.Run(mediaType, func(t *testing.T) {
			req, resp := cachePolicyFixture()
			req.URL.Path = "/v2/team/image/manifests/" + testBlobDigest
			resp.Header.Set("Content-Type", mediaType+`; profile="exact"`)
			meta, ok := objectMetadata(req, resp)
			if !ok || meta.MediaType != mediaType+`; profile="exact"` {
				t.Fatalf("metadata = %+v, %v", meta, ok)
			}
		})
	}

	for _, mediaType := range []string{"application/json", "application/vnd.docker.distribution.manifest.v1+json"} {
		req, resp := cachePolicyFixture()
		req.URL.Path = "/v2/team/image/manifests/" + testBlobDigest
		resp.Header.Set("Content-Type", mediaType)
		if _, ok := objectMetadata(req, resp); ok {
			t.Errorf("unsupported manifest media type %q admitted", mediaType)
		}
	}
}

func TestBlobMetadataRejectsUnsafeHeadersAndFraming(t *testing.T) {
	tests := map[string]func(*http.Request, *http.Response){
		"request no-store case and parameters":  func(r *http.Request, _ *http.Response) { r.Header.Add("Cache-Control", "Public, No-Store=\"reason\"") },
		"response no-store case and parameters": func(_ *http.Request, r *http.Response) { r.Header.Add("Cache-Control", "max-age=60, NO-STORE=reason") },
		"vary authorization":                    func(_ *http.Request, r *http.Response) { r.Header.Set("Vary", "Accept, Authorization") },
		"vary star":                             func(_ *http.Request, r *http.Response) { r.Header.Set("Vary", "*") },
		"absent media":                          func(_ *http.Request, r *http.Response) { r.Header.Del("Content-Type") },
		"duplicate media": func(_ *http.Request, r *http.Response) {
			r.Header["Content-Type"] = []string{"application/octet-stream", "application/json"}
		},
		"invalid media": func(_ *http.Request, r *http.Response) { r.Header.Set("Content-Type", "not a media type") },
		"nonidentity encoding": func(_ *http.Request, r *http.Response) {
			r.Header.Set("Content-Encoding", "gzip")
		},
		"duplicate encoding": func(_ *http.Request, r *http.Response) {
			r.Header["Content-Encoding"] = []string{"identity", "identity"}
		},
		"partial content-range": func(_ *http.Request, r *http.Response) {
			r.Header.Set("Content-Range", "bytes 0-2/10")
		},
		"trailers":       func(_ *http.Request, r *http.Response) { r.Trailer = http.Header{"X-Checksum": {"later"}} },
		"unknown length": func(_ *http.Request, r *http.Response) { r.ContentLength = -1 },
		"mismatching digest": func(_ *http.Request, r *http.Response) {
			r.Header.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
		},
		"conflicting digest lines": func(_ *http.Request, r *http.Response) {
			r.Header["Docker-Content-Digest"] = []string{testBlobDigest, "sha256:" + strings.Repeat("b", 64)}
		},
		"invalid digest": func(_ *http.Request, r *http.Response) { r.Header.Set("Docker-Content-Digest", "invalid") },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			req, resp := cachePolicyFixture()
			change(req, resp)
			if _, ok := objectMetadata(req, resp); ok {
				t.Fatal("unsafe response was cacheable")
			}
		})
	}
}

func TestBlobMetadataRejectsUnsupportedResponses(t *testing.T) {
	for _, status := range []int{206, 304, 416, 401, 403, 404, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			req, resp := cachePolicyFixture()
			resp.StatusCode = status
			if _, ok := objectMetadata(req, resp); ok {
				t.Fatalf("status %d was cacheable", status)
			}
		})
	}
}

func TestBlobMetadataKeyIsolation(t *testing.T) {
	key := func(change func(*http.Request, *http.Response)) string {
		req, resp := cachePolicyFixture()
		change(req, resp)
		meta, ok := objectMetadata(req, resp)
		if !ok {
			t.Fatalf("key fixture was rejected")
		}
		return meta.Key
	}
	base := key(func(r *http.Request, _ *http.Response) { r.Header["Accept"] = []string{"secret/one", "secret/two"} })

	canonical := key(func(r *http.Request, _ *http.Response) {
		r.URL.Host = "REGISTRY.EXAMPLE.:0443"
		r.Header["Accept"] = []string{"secret/one", "secret/two"}
	})
	if canonical != base {
		t.Fatal("canonical equivalent authorities produced different keys")
	}

	variants := map[string]func(*http.Request, *http.Response){
		"port": func(r *http.Request, _ *http.Response) {
			r.URL.Host = "registry.example:5000"
			r.Header["Accept"] = []string{"secret/one", "secret/two"}
		},
		"repository": func(r *http.Request, _ *http.Response) {
			r.URL.Path = "/v2/other/image/blobs/" + testBlobDigest
			r.Header["Accept"] = []string{"secret/one", "secret/two"}
		},
		"accept concatenation": func(r *http.Request, _ *http.Response) {
			r.Header["Accept"] = []string{"secret/one, secret/two"}
		},
		"accept values": func(r *http.Request, _ *http.Response) { r.Header["Accept"] = []string{"secret/one", "secret/three"} },
		"media type": func(r *http.Request, p *http.Response) {
			r.Header["Accept"] = []string{"secret/one", "secret/two"}
			p.Header.Set("Content-Type", "application/json")
		},
	}
	for name, change := range variants {
		t.Run(name, func(t *testing.T) {
			if got := key(change); got == base {
				t.Fatal("distinct cache dimensions produced the same key")
			}
		})
	}
	sharedMedia := "application/vnd.oci.image.manifest.v1+json"
	blobKey := key(func(r *http.Request, p *http.Response) {
		r.Header["Accept"] = []string{"secret/one", "secret/two"}
		p.Header.Set("Content-Type", sharedMedia)
	})
	manifestKey := key(func(r *http.Request, p *http.Response) {
		r.URL.Path = "/v2/team/image/manifests/" + testBlobDigest
		r.Header["Accept"] = []string{"secret/one", "secret/two"}
		p.Header.Set("Content-Type", sharedMedia)
	})
	if blobKey == manifestKey {
		t.Error("object kind did not isolate otherwise identical keys")
	}
	if strings.Contains(base, "secret") || len(base) != 64 {
		t.Fatalf("cache key exposes raw header data: %q", base)
	}
}

func TestManifestMediaTypeMatchesCanonicalListExactly(t *testing.T) {
	want := []string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}
	if !slices.Equal(manifestMediaTypes, want) {
		t.Fatalf("manifestMediaTypes = %q, want %q", manifestMediaTypes, want)
	}

	for _, mediaType := range manifestMediaTypes {
		if !manifestMediaType(mediaType) {
			t.Errorf("manifestMediaType(%q) = false, want true", mediaType)
		}
	}
	for _, mediaType := range []string{
		"",
		"application/json",
		"application/vnd.oci.image.manifest.v1+json ",
		"Application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.v1+json",
		"application/octet-stream",
	} {
		if manifestMediaType(mediaType) {
			t.Errorf("manifestMediaType(%q) = true, want false", mediaType)
		}
	}
}

func TestCacheHeaderRejectionAgreesWithCacheHeaders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     http.Header
		wantReason string
	}{
		{"cacheable empty", http.Header{}, ""},
		{"cacheable vary accept", http.Header{"Vary": {"Accept"}}, ""},
		{"cacheable matching digest", http.Header{"Docker-Content-Digest": {testBlobDigest}}, ""},
		{"no store", http.Header{"Cache-Control": {"no-store"}}, "no_store"},
		{"vary accept and encoding", http.Header{"Vary": {"Accept, Accept-Encoding"}}, "vary"},
		{"vary other", http.Header{"Vary": {"Cookie"}}, "vary"},
		{"digest mismatch", http.Header{"Docker-Content-Digest": {"sha256:" + strings.Repeat("b", 64)}}, "digest_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := cacheHeaderRejection(tc.header, testBlobDigest)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if got, want := cacheHeaders(tc.header, testBlobDigest), tc.wantReason == ""; got != want {
				t.Fatalf("cacheHeaders = %v, but reason %q implies %v", got, reason, want)
			}
		})
	}
}

func TestCachedResponseRecordsVaryAndDigestBypassReasons(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(http.Header)
		wantReason string
	}{
		{
			name:       "vary",
			mutate:     func(h http.Header) { h.Set("Vary", "Accept, Accept-Encoding") },
			wantReason: "vary",
		},
		{
			name:       "digest mismatch",
			mutate:     func(h http.Header) { h.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("c", 64)) },
			wantReason: "digest_mismatch",
		},
		{
			name:       "no reason for plain uncacheable",
			mutate:     func(h http.Header) { h.Set("Cache-Control", "no-store") },
			wantReason: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), cache.Limits{MaxBytes: 1 << 20, MaxFills: 4, MaxEntries: 8, FillTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			transport := registryTransport{cache: store}
			req, resp := cachePolicyFixture()
			req = req.WithContext(context.Background())
			resp.Body = io.NopCloser(strings.NewReader("abc"))
			tc.mutate(resp.Header)
			stats := &requestStats{}
			got, retry, err := transport.resolveResponse(req, resp, true, false, stats)
			if got == nil || retry || err != nil {
				t.Fatalf("resolveResponse = (%v, %v, %v), want the upstream response passed through", got, retry, err)
			}
			if stats.reject != tc.wantReason {
				t.Fatalf("stats.reject = %q, want %q", stats.reject, tc.wantReason)
			}
			// The upstream body must be untouched by the bypass.
			body, _ := io.ReadAll(got.Body)
			got.Body.Close()
			if string(body) != "abc" {
				t.Fatalf("bypass altered upstream body: %q", body)
			}
		})
	}
}

func TestBlobClassification(t *testing.T) {
	for _, tc := range []struct {
		path, method, header string
		want                 bool
	}{
		{testBlobPath, "GET", "", true},
		{testBlobPath, "HEAD", "", false},
		{testBlobPath, "GET", "Range", false},
		{testBlobPath, "GET", "If-None-Match", false},
		{testBlobPath + "?", "GET", "", false},
		{testBlobPath + "?secret=x", "GET", "", false},
		{strings.Replace(testBlobPath, "team/", "team%2f", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "team/", "team/../", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "sha256", "sha512", 1), "GET", "", false},
		{strings.Replace(testBlobPath, "blobs", "manifests", 1), "GET", "", true},
	} {
		r, _ := http.NewRequest(tc.method, "https://registry.test"+tc.path, nil)
		if tc.header != "" {
			r.Header[tc.header] = []string{""}
		}
		_, got := immutableObjectRequest(r)
		if got != tc.want {
			t.Errorf("%s %s %s: %v", tc.method, tc.path, tc.header, got)
		}
	}
}

func FuzzImmutableObjectRequest(f *testing.F) {
	f.Add(testBlobPath)
	f.Add("/v2/repo/manifests/latest")
	f.Fuzz(func(t *testing.T, path string) {
		r := &http.Request{Method: "GET", URL: &url.URL{Path: path}, Header: make(http.Header)}
		object, ok := immutableObjectRequest(r)
		if ok && (object.repository == "" || len(object.digest) != 71 || strings.Contains(path, "..")) {
			t.Fatal("invalid classification")
		}
	})
}
