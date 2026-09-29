package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/hostport"
)

func TestWarmCacheRequiresEachClientsGETAuthorization(t *testing.T) {
	const body = "authorized blob bytes"
	var gets, heads, allowed atomic.Int32
	var revoked atomic.Bool
	var failure atomic.Int32
	var originBytes atomic.Int64
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			heads.Add(1)
			blobHeaders(w, body)
			return
		}
		gets.Add(1)
		if code := int(failure.Load()); code != 0 {
			w.WriteHeader(code)
			io.WriteString(w, "upstream failure")
			return
		}
		token := r.Header.Get("Authorization")
		if token != "Bearer alice" && token != "Bearer bob" || token == "Bearer alice" && revoked.Load() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.test/token"`)
			w.WriteHeader(401)
			io.WriteString(w, "denied")
			return
		}
		n := allowed.Add(1)
		blobHeaders(w, body)
		w.Header().Set("Set-Cookie", fmt.Sprintf("current=%d", n))
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if n == 1 {
			n, _ := io.WriteString(w, body)
			originBytes.Add(int64(n))
			return
		}
		// A warm response has headers but deliberately never sends body bytes.
		// Only substitution followed by prompt upstream cancellation can succeed.
		<-r.Context().Done()
	}))
	defer origin.Close()
	var logs bytes.Buffer
	p, c := cacheFixture(t, origin, 1<<20, &logs)
	u := origin.URL + blobURL(body)
	for i, token := range []string{"Bearer alice", "Bearer bob", "Bearer alice"} {
		resp, got := getBlob(t, c, u, token)
		if resp.StatusCode != 200 || got != body || resp.Header.Get("Set-Cookie") != fmt.Sprintf("current=%d", i+1) {
			t.Fatalf("response %d %q", resp.StatusCode, got)
		}
	}
	if gets.Load() != 3 || originBytes.Load() != int64(len(body)) {
		t.Fatalf("GETs=%d bytes=%d", gets.Load(), originBytes.Load())
	}
	revoked.Store(true)
	for _, token := range []string{"Bearer stranger", "Bearer alice"} {
		resp, got := getBlob(t, c, u, token)
		if resp.StatusCode != 401 || got != "denied" || resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("unauthorized cache delivery")
		}
	}
	r, _ := http.NewRequest("HEAD", u, nil)
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || heads.Load() != 1 {
		t.Fatal("public HEAD failed")
	}
	resp, got := getBlob(t, c, u, "")
	if resp.StatusCode != 401 || got != "denied" {
		t.Fatal("HEAD authorized GET")
	}
	for _, code := range []int32{403, 404, 429, 500, 503} {
		failure.Store(code)
		resp, got := getBlob(t, c, u, "Bearer bob")
		if resp.StatusCode != int(code) || got != "upstream failure" {
			t.Fatalf("cache replaced %d", code)
		}
	}
	origin.Close()
	resp, got = getBlob(t, c, u, "Bearer bob")
	if resp.StatusCode != 502 || got == body {
		t.Fatal("outage served cache")
	}
	shutdown(t, p)
	if !strings.Contains(logs.String(), "cache=hit") || !strings.Contains(logs.String(), "fill=stored") {
		t.Fatal("missing cache diagnostics")
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `msg="registry request"`) && !strings.Contains(line, "url="+u+" ") {
			t.Fatalf("request log missing URL: %s", line)
		}
	}
	for _, secret := range []string{"Bearer alice", "Bearer bob", "current="} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logged %s", secret)
		}
	}
}

func TestWarmMetadataMismatchAndBypassesForwardOriginBody(t *testing.T) {
	const body = "good"
	for _, mode := range []string{"no-store", "request-no-store-hop", "range-hop", "vary", "encoding", "digest", "media", "length", "HEAD", "If-Range", "conditional", "tag", "query", "206", "304", "416"} {
		t.Run(mode, func(t *testing.T) {
			var warm atomic.Bool
			var calls atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				out := body
				if warm.Load() {
					out = "evil"
				}
				blobHeaders(w, out)
				if warm.Load() {
					switch mode {
					case "no-store":
						w.Header().Set("Cache-Control", "no-store")
					case "vary":
						w.Header().Set("Vary", "Authorization")
					case "encoding":
						w.Header().Set("Content-Encoding", "custom")
					case "digest":
						w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64))
					case "media":
						w.Header().Set("Content-Type", "application/json")
					case "length":
						out = "different length"
						w.Header().Set("Content-Length", fmt.Sprint(len(out)))
					case "206":
						w.WriteHeader(206)
					case "304":
						w.WriteHeader(304)
						return
					case "416":
						w.WriteHeader(416)
					}
				}
				io.WriteString(w, out)
			}))
			defer origin.Close()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			u := origin.URL + blobURL(body)
			getBlob(t, c, u, "")
			warm.Store(true)
			method := "GET"
			if mode == "HEAD" {
				method = "HEAD"
			}
			if mode == "query" {
				u += "?namespace=other"
			}
			if mode == "tag" {
				u = origin.URL + "/v2/team/image/manifests/latest"
			}
			r, _ := http.NewRequest(method, u, nil)
			if mode == "If-Range" {
				r.Header.Set("Range", "bytes=0-3")
				r.Header.Set("If-Range", `"version"`)
			}
			if mode == "conditional" {
				r.Header.Set("If-Match", `"version"`)
			}
			if mode == "request-no-store-hop" {
				r.Header.Set("Cache-Control", "no-store")
				r.Header.Set("Connection", "Cache-Control")
			}
			if mode == "range-hop" {
				r.Header.Set("Range", "bytes=0-3")
				r.Header.Set("Connection", "Range")
			}
			resp, err := c.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := "evil"
			if mode == "HEAD" || mode == "304" {
				want = ""
			}
			if mode == "length" {
				want = "different length"
			}
			if string(got) != want || calls.Load() != 2 {
				t.Fatalf("body=%q calls=%d", got, calls.Load())
			}
			if p.cache.Stats().Entries != 1 {
				t.Fatal("bypass poisoned cache")
			}
		})
	}
}

func TestRedirectedCacheStoresIdentityBlobVaryingOnAcceptEncoding(t *testing.T) {
	const body = "good"
	for _, tc := range []struct {
		name       string
		encoding   string
		grants     bool
		wantBody   string
		wantStore  int
		wantGets   int32
		wantReject string
	}{
		{name: "identity", wantBody: body, wantStore: 1, wantGets: 2, wantReject: `reject=""`},
		{name: "identity with grants", grants: true, wantBody: body, wantStore: 1, wantGets: 1, wantReject: `reject=""`},
		{name: "gzip", encoding: "gzip", wantBody: "evil", wantGets: 2, wantReject: "reject=vary"},
		{name: "gzip with grants", encoding: "gzip", grants: true, wantBody: "evil", wantGets: 2, wantReject: "reject=vary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, storage atomic.Int32
			// Like CloudFront, advertise Vary: Accept-Encoding for a gzip-accepting client.
			cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				out := body
				if storage.Add(1) > 1 {
					out = "evil"
				}
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Errorf("Accept-Encoding = %q, want client value forwarded", r.Header.Get("Accept-Encoding"))
				}
				blobHeaders(w, out)
				w.Header().Set("Vary", "Accept-Encoding")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				io.WriteString(w, out)
			}))
			defer cdn.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", cdn.URL+"/signed")
				w.WriteHeader(307)
			}))
			defer origin.Close()
			var logs bytes.Buffer
			p, c := cacheFixture(t, origin, 1<<20, &logs)
			if tc.grants {
				p.authorization = newAuthorizationCache(time.Minute, 10)
			}
			a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
			p.destinations[a.String()] = struct{}{}
			p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())

			u := origin.URL + blobURL(body)
			var got []string
			for i := 0; i < 2; i++ {
				r, _ := http.NewRequest("GET", u, nil)
				r.Header.Set("Authorization", "Bearer client")
				// Set explicitly, as Docker's client does, which also disables transparent decompression.
				r.Header.Set("Accept-Encoding", "gzip")
				resp, err := c.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, string(b))
			}
			if got[0] != body || got[1] != tc.wantBody {
				t.Fatalf("bodies = %q, want [%q %q]", got, body, tc.wantBody)
			}
			// Without a grant every GET is authorized at each hop, and a hit replaces only
			// the storage body; a live grant serves the second GET with no upstream I/O.
			if requests.Load() != tc.wantGets || storage.Load() != tc.wantGets {
				t.Fatalf("registry/storage requests = %d/%d, want %d each", requests.Load(), storage.Load(), tc.wantGets)
			}
			if got := p.cache.Stats().Entries; got != tc.wantStore {
				t.Fatalf("cache entries = %d, want %d", got, tc.wantStore)
			}
			shutdown(t, p)
			if n := strings.Count(logs.String(), tc.wantReject); n != 2 {
				t.Fatalf("logs contain %q %d times, want 2: %s", tc.wantReject, n, logs.String())
			}
		})
	}
}

func TestRedirectedCacheAuthorizesCDNAndHonorsRegistryNoStore(t *testing.T) {
	const body = "good"
	var requests, storage atomic.Int32
	var deny, noStore atomic.Bool
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := storage.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials reached CDN")
		}
		if deny.Load() {
			w.WriteHeader(403)
			io.WriteString(w, "denied")
			return
		}
		blobHeaders(w, body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if n == 1 {
			io.WriteString(w, body)
		} else if noStore.Load() {
			io.WriteString(w, "evil")
		} else {
			<-r.Context().Done()
		}
	}))
	defer cdn.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if noStore.Load() {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("Location", cdn.URL+"/signed?secret=temporary")
		w.WriteHeader(307)
	}))
	defer origin.Close()
	var logs bytes.Buffer
	p, c := cacheFixture(t, origin, 1<<20, &logs)
	a, _ := hostport.Parse(strings.TrimPrefix(cdn.URL, "https://"), "443")
	p.destinations[a.String()] = struct{}{}
	p.transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
	u := origin.URL + blobURL(body)
	for i := 0; i < 2; i++ {
		_, got := getBlob(t, c, u, "Bearer client")
		if got != body {
			t.Fatal(got)
		}
	}
	deny.Store(true)
	resp, got := getBlob(t, c, u, "Bearer client")
	if resp.StatusCode != 403 || got != "denied" {
		t.Fatal("CDN denial replaced")
	}
	deny.Store(false)
	noStore.Store(true)
	_, got = getBlob(t, c, u, "Bearer client")
	if got != "evil" {
		t.Fatal("registry no-store ignored")
	}
	if requests.Load() != 4 || storage.Load() != 4 {
		t.Fatal("missing per-hop authorization")
	}
	shutdown(t, p)
	if got := strings.Count(logs.String(), "url="+u+" "); got != 4 {
		t.Fatalf("expected original URL in four completion logs, got %d: %s", got, logs.String())
	}
	for _, secret := range []string{cdn.URL, "/signed", "temporary", "Bearer client"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain redirect or credential %q", secret)
		}
	}
	if err := filepath.WalkDir(p.cfg.Cache.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if bytes.Contains(data, []byte("secret=temporary")) || bytes.Contains(data, []byte("Bearer client")) {
			t.Error("persisted credentials or redirect URL")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFillCancellationAndCacheFailurePreserveTransfer(t *testing.T) {
	for _, mode := range []string{"cancel", "shutdown", "cache-close", "truncated", "digest", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			const body = "abcd"
			ready, release := make(chan struct{}), make(chan struct{})
			cancelled := make(chan struct{})
			var once sync.Once
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blobHeaders(w, body)
				w.WriteHeader(200)
				io.WriteString(w, "a")
				w.(http.Flusher).Flush()
				once.Do(func() { close(ready) })
				select {
				case <-release:
				case <-r.Context().Done():
					close(cancelled)
					return
				}
				if mode == "truncated" {
					return
				}
				if mode == "digest" {
					io.WriteString(w, "xxx")
				} else {
					io.WriteString(w, "bcd")
				}
			}))
			defer origin.Close()
			limit := int64(1 << 20)
			if mode == "capacity" {
				limit = 1
			}
			p, c := cacheFixture(t, origin, limit, nil)
			r, _ := http.NewRequest("GET", origin.URL+blobURL(body), nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r = r.WithContext(ctx)
			resp, err := c.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			<-ready
			if mode == "cache-close" {
				if err := p.cache.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel" {
				cancel()
				resp.Body.Close()
			}
			if mode == "shutdown" {
				ctx, done := context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer done()
				// Upstream idle expiry may have already aborted the stream;
				// otherwise this deadline forces it closed.
				if err := p.Shutdown(ctx); err != nil && err != context.DeadlineExceeded {
					t.Fatalf("Shutdown = %v, want nil or deadline exceeded", err)
				}
			}
			if mode == "cancel" || mode == "shutdown" {
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("origin not cancelled")
				}
			}
			close(release)
			got, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if mode == "capacity" || mode == "cache-close" {
				if readErr != nil || string(got) != body {
					t.Fatalf("cache failed transfer %q %v", got, readErr)
				}
			}
			if mode == "digest" && (readErr != nil || string(got) != "axxx") {
				t.Fatal("cache altered origin bytes")
			}
			if mode == "truncated" && readErr == nil {
				t.Fatal("truncation hidden")
			}
			deadline := time.Now().Add(time.Second)
			for p.cache.Stats().Fills != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s := p.cache.Stats(); s.Fills != 0 || s.Reserved != 0 || s.Entries != 0 {
				t.Fatalf("incomplete fill published/leaked %+v", s)
			}
			awaitNoActiveRequests(t, p)
			m := p.metrics.snapshot()
			wantBytes, wantEntries := uint64(len(body)), uint64(1)
			if mode == "truncated" || mode == "cancel" || mode == "shutdown" {
				wantBytes, wantEntries = 1, 0
			}
			if m.servedBytes["origin"] != wantBytes || m.servedEntries["origin"] != wantEntries || m.servedBytes["cache"] != 0 || m.servedEntries["cache"] != 0 {
				t.Fatalf("failed fill delivery metrics = %+v, want origin bytes=%d entries=%d", m, wantBytes, wantEntries)
			}
		})
	}
}

func TestConcurrentColdClientsAuthorizeIndependently(t *testing.T) {
	const clients = 12
	const body = "concurrent blob"
	var calls atomic.Int32
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobHeaders(w, body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if calls.Add(1) == clients {
			close(ready)
		}
		select {
		case <-release:
			io.WriteString(w, body)
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	results := make(chan error, clients)
	for i := 0; i < clients; i++ {
		go func() {
			resp, err := c.Get(origin.URL + blobURL(body))
			if err == nil {
				var b []byte
				b, err = io.ReadAll(resp.Body)
				resp.Body.Close()
				if err == nil && string(b) != body {
					err = fmt.Errorf("unexpected bytes")
				}
			}
			results <- err
		}()
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("clients shared authorization or blocked on fill")
	}
	once.Do(func() { close(release) })
	for i := 0; i < clients; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != clients {
		t.Fatal("wrong GET count")
	}
	if s := p.cache.Stats(); s.Entries != 1 || s.Fills != 0 || s.Reserved != 0 {
		t.Fatalf("bad concurrent storage %+v", s)
	}
}

func TestCacheSeparatesRepositoriesAndRegistries(t *testing.T) {
	const body = "good"
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobHeaders(w, body)
		if strings.Contains(r.URL.Path, "/other/") {
			io.WriteString(w, "evil")
		} else {
			io.WriteString(w, body)
		}
	}))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	getBlob(t, c, origin.URL+blobURL(body), "")
	_, got := getBlob(t, c, origin.URL+strings.Replace(blobURL(body), "team/image", "other/image", 1), "")
	if got != "evil" {
		t.Fatal("cross-repository substitution")
	}
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { blobHeaders(w, body); io.WriteString(w, "evil") }))
	defer other.Close()
	p2, _, c2 := interceptFixture(t, other, nil, true)
	p2.cache = p.cache
	_, got = getBlob(t, c2, other.URL+blobURL(body), "")
	if got != "evil" {
		t.Fatal("cross-registry substitution")
	}
}

func TestCacheLimitsPreserveUpstreamStream(t *testing.T) {
	for _, mode := range []string{"fill-cap", "entry-cap", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limits := cache.Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 2, FillTimeout: time.Second}
				if mode == "entry-cap" {
					limits.MaxFills, limits.MaxEntries = 2, 1
				}
				s, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), limits)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				const body = "all upstream bytes survive cache admission failure"
				req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://registry.test"+blobURL(body), nil)
				response := func() *http.Response {
					return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
				}
				meta, ok := objectMetadata(req, response())
				if !ok {
					t.Fatal("invalid fixture")
				}
				if mode != "expiry" {
					other := meta
					other.Key = strings.Repeat("0", 64)
					f, err := s.Begin(other)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Abort()
				}
				transport := registryTransport{cache: s}
				stats := &requestStats{}
				resp, retry, err := transport.resolveResponse(req, response(), true, false, stats)
				if err != nil || retry || resp == nil {
					t.Fatalf("resolveResponse = (%v, %v, %v), want response without retry", resp, retry, err)
				}
				first := make([]byte, 3)
				if _, err := io.ReadFull(resp.Body, first); err != nil {
					t.Fatal(err)
				}
				if mode == "expiry" {
					time.Sleep(2 * time.Second)
					synctest.Wait()
					if st := s.Stats(); st.Fills != 0 || st.Reserved != 0 {
						t.Fatalf("expiry did not reclaim fill: %+v", st)
					}
				}
				rest, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(first)+string(rest) != body || stats.upstreamBytes != int64(len(body)) {
					t.Fatalf("upstream stream changed: %v %+v", err, stats)
				}
				if st := s.Stats(); st.Entries != 0 {
					t.Fatalf("partial entry published: %+v", st)
				}
			})
		})
	}
}
