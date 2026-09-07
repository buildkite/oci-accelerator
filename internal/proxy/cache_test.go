package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
	"ampcode.com/buildkite/docker-accelerator/internal/hostport"
)

func cacheFixture(t *testing.T, origin *httptest.Server, limit int64, logs *bytes.Buffer) (*Proxy, *http.Client) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	s, err := cache.Open(dir, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	p, _, c := interceptFixture(t, origin, logs, true)
	p.cache = s
	p.cfg.Cache.Dir = dir
	return p, c
}
func blobURL(body string) string {
	return fmt.Sprintf("/v2/team/image/blobs/sha256:%x", sha256.Sum256([]byte(body)))
}
func blobHeaders(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
}
func getBlob(t *testing.T, c *http.Client, u, token string) (*http.Response, string) {
	t.Helper()
	r, _ := http.NewRequest("GET", u, nil)
	r.Header.Set("Authorization", token)
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

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
	for _, secret := range []string{"Bearer alice", "Bearer bob", "current=", "team/image"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logged %s", secret)
		}
	}
}

func TestWarmMetadataMismatchAndBypassesForwardOriginBody(t *testing.T) {
	const body = "good"
	for _, mode := range []string{"no-store", "request-no-store-hop", "range-hop", "vary", "encoding", "digest", "media", "length", "HEAD", "Range", "conditional", "tag", "query", "206", "304", "416"} {
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
			if mode == "Range" {
				r.Header.Set("Range", "bytes=0-3")
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
	p, c := cacheFixture(t, origin, 1<<20, nil)
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
				p.Shutdown(ctx)
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
