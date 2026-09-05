package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ampcode.com/buildkite/docker-accelerator/internal/authority"
)

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
	a, _ := authority.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
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
