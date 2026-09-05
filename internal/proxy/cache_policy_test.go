package proxy

import (
	"net/http"
	"strings"
	"testing"
)

const testBlobDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func cachePolicyFixture() (*http.Request, *http.Response) {
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example/v2/team/image/blobs/"+testBlobDigest, nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		ContentLength: 3,
	}
	resp.Header.Set("Content-Type", "application/octet-stream")
	resp.Header.Set("Content-Encoding", "identity")
	return req, resp
}

func TestBlobMetadataAcceptsTerminalBlobResponse(t *testing.T) {
	req, resp := cachePolicyFixture()
	resp.Header.Set("Docker-Content-Digest", testBlobDigest)
	meta, ok := blobMetadata(req, resp)
	if !ok {
		t.Fatal("matching terminal blob response was rejected")
	}
	if meta.Digest != testBlobDigest || meta.Size != 3 || meta.MediaType != "application/octet-stream" || meta.Key == "" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	resp.Header.Del("Docker-Content-Digest")
	if _, ok := blobMetadata(req, resp); !ok {
		t.Fatal("response without optional CDN digest was rejected")
	}
}

func TestBlobMetadataRejectsUnsupportedRequests(t *testing.T) {
	tests := map[string]func(*http.Request){
		"tag manifest":       func(r *http.Request) { r.URL.Path = "/v2/team/image/manifests/latest" },
		"digest manifest":    func(r *http.Request) { r.URL.Path = "/v2/team/image/manifests/" + testBlobDigest },
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
			if _, ok := blobMetadata(req, resp); ok {
				t.Fatal("unsupported request was cacheable")
			}
		})
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
			if _, ok := blobMetadata(req, resp); ok {
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
			if _, ok := blobMetadata(req, resp); ok {
				t.Fatalf("status %d was cacheable", status)
			}
		})
	}
}

func TestBlobMetadataKeyIsolation(t *testing.T) {
	key := func(change func(*http.Request, *http.Response)) string {
		req, resp := cachePolicyFixture()
		change(req, resp)
		meta, ok := blobMetadata(req, resp)
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
	if strings.Contains(base, "secret") || len(base) != 64 {
		t.Fatalf("cache key exposes raw header data: %q", base)
	}
}
