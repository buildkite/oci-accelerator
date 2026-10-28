package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/ca"
)

const (
	registryUser = "fixture"
	registryPass = "acceptance-only"
)

type registryCounts struct {
	mu       sync.Mutex
	authed   map[string]int
	bytes    map[string]int64
	tags     map[string]int
	active   int
	badAuth  int
	requests int

	// chunkGate optionally pauses large responses after each flushed chunk.
	chunkGate <-chan struct{}
}

func (c *registryCounts) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("origin requests did not finish")
}

func (c *registryCounts) tagCount(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tags[path]
}

func (c *registryCounts) snapshot() (map[string]int, map[string]int64, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, b := map[string]int{}, map[string]int64{}
	for k, v := range c.authed {
		a[k] = v
	}
	for k, v := range c.bytes {
		b[k] = v
	}
	return a, b, c.badAuth, c.requests
}

func startRegistry(t *testing.T, root string, ip net.IP, image fixtureImage) (*httptestServer, fixtureImage, *registryCounts) {
	t.Helper()
	counts := &registryCounts{authed: map[string]int{}, bytes: map[string]int64{}, tags: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		registryReply(counts, w, r, nil, "")
	})
	mux.HandleFunc("/v2/tiny/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.manifest, "application/vnd.docker.distribution.manifest.v2+json")
	})
	mux.HandleFunc("/v2/tiny/manifests/"+image.manifestDigest, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.manifest, "application/vnd.docker.distribution.manifest.v2+json")
	})
	mux.HandleFunc(image.layerPath, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.layer, "application/octet-stream")
	})
	mux.HandleFunc(image.configPath, func(w http.ResponseWriter, r *http.Request) {
		registryReply(counts, w, r, image.config, "application/octet-stream")
	})

	a, err := ca.Open(filepath.Join(root, "origin-ca"), 8)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := a.Certificate(ip.String())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts.mu.Lock()
		counts.requests++
		counts.active++
		counts.mu.Unlock()
		defer func() {
			counts.mu.Lock()
			counts.active--
			counts.mu.Unlock()
		}()
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.Serve(tlsLn) }()
	return &httptestServer{Server: s, Listener: tlsLn}, image, counts
}

type httptestServer struct {
	Server   *http.Server
	Listener net.Listener
}

func (s *httptestServer) Close() { _ = s.Server.Close() }

func registryReply(c *registryCounts, w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	u, p, ok := r.BasicAuth()
	if !ok || u != registryUser || p != registryPass {
		c.mu.Lock()
		if r.Header.Get("Authorization") != "" {
			c.badAuth++
		}
		c.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		c.mu.Lock()
		c.authed[r.URL.Path]++
		c.mu.Unlock()
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasSuffix(r.URL.Path, "/manifests/latest") {
		c.mu.Lock()
		c.tags[r.URL.Path]++
		c.mu.Unlock()
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if len(body) != 0 {
		w.Header().Set("Docker-Content-Digest", digest(body))
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	chunkSize := len(body)
	if len(body) >= 1<<20 {
		chunkSize = 32 << 10
	}
	for offset := 0; offset < len(body); offset += chunkSize {
		end := min(offset+chunkSize, len(body))
		select {
		case <-r.Context().Done():
			return
		default:
		}
		n, err := w.Write(body[offset:end])
		c.mu.Lock()
		c.bytes[r.URL.Path] += int64(n)
		c.mu.Unlock()
		if err != nil {
			return
		}
		if len(body) >= 1<<20 {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if c.chunkGate != nil {
				select {
				case <-c.chunkGate:
				case <-r.Context().Done():
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestRegistryBodyCountersExcludeHEAD(t *testing.T) {
	c := &registryCounts{authed: map[string]int{}, bytes: map[string]int64{}, tags: map[string]int{}}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := httptest.NewRequest(method, "/v2/tiny/blobs/object", nil)
		r.SetBasicAuth(registryUser, registryPass)
		w := httptest.NewRecorder()
		registryReply(c, w, r, []byte("body"), "application/octet-stream")
		a, b, bad, _ := c.snapshot()
		want := 0
		if method == http.MethodGet {
			want = 1
		}
		if w.Code != http.StatusOK || a[r.URL.Path] != want || b[r.URL.Path] != int64(want*4) || bad != 0 {
			t.Fatalf("%s: status=%d counts=%v bytes=%v badAuth=%d", method, w.Code, a, b, bad)
		}
	}
}

func TestRegistryStreamingCancellationCountsWrittenBytes(t *testing.T) {
	c := &registryCounts{authed: map[string]int{}, bytes: map[string]int64{}, tags: map[string]int{}}
	// Keep the handler at the first chunk until the request is canceled.
	c.chunkGate = make(chan struct{})
	body := deterministicLargePayload()
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		registryReply(c, w, r, body, "application/octet-stream")
	}))
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/large", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(registryUser, registryPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, body[:len(buf)]) {
		t.Fatal("streamed prefix does not match the response body")
	}

	cancel()
	_ = resp.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streaming handler did not observe cancellation")
	}
	a, b, bad, _ := c.snapshot()
	if a["/large"] != 1 || b["/large"] != int64(len(buf)) || bad != 0 {
		t.Fatalf("canceled stream requests=%d bytes=%d body=%d badAuth=%d", a["/large"], b["/large"], len(body), bad)
	}
}
