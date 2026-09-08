package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

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
				p := &Proxy{cache: s}
				stats := &requestStats{}
				resp, retry, err := p.cachedResponse(req, response(), true, false, stats)
				if err != nil || retry || resp == nil {
					t.Fatalf("cachedResponse = (%v, %v, %v), want response without retry", resp, retry, err)
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
