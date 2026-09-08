package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

type closeCallbackBody struct {
	io.ReadCloser
	close func()
}

func (b *closeCallbackBody) Close() error {
	b.close()
	return b.ReadCloser.Close()
}

func TestCacheFillWaitRetriesWhenCompletedEntryIsEvictedBeforeGet(t *testing.T) {
	const body = "entry evicted between completion and get"
	limits := cache.Limits{MaxBytes: 1 << 20, MaxFills: 2, MaxEntries: 1, FillTimeout: time.Minute}
	s, err := cache.OpenWithLimits(filepath.Join(t.TempDir(), "cache"), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://registry.test"+blobURL(body), nil)
	response := func(b io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: b}
	}
	meta, ok := objectMetadata(req, response(io.NopCloser(strings.NewReader(body))))
	if !ok {
		t.Fatal("invalid fixture metadata")
	}
	leader, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(leader, body); err != nil {
		t.Fatal(err)
	}

	otherBody := "replacement entry"
	other := cache.Metadata{Key: strings.Repeat("0", 64), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(otherBody))), Size: int64(len(otherBody)), MediaType: "application/octet-stream"}
	var once sync.Once
	upstream := &closeCallbackBody{ReadCloser: io.NopCloser(strings.NewReader(body)), close: func() {
		once.Do(func() {
			if err := leader.Commit(); err != nil {
				t.Errorf("commit leader: %v", err)
				return
			}
			replacement, err := s.Begin(other)
			if err != nil {
				t.Errorf("begin replacement: %v", err)
				return
			}
			if _, err := io.WriteString(replacement, otherBody); err != nil {
				t.Errorf("write replacement: %v", err)
				return
			}
			if err := replacement.Commit(); err != nil {
				t.Errorf("commit replacement: %v", err)
			}
		})
	}}
	p := &Proxy{cache: s}
	p.cfg.Cache.FillWaitTimeout = time.Second
	stats := &requestStats{}
	got, retry, err := p.cachedResponse(req, response(upstream), true, true, stats)
	if got != nil || !retry || err != nil {
		t.Fatalf("cachedResponse = (%v, %v, %v), want (nil, true, nil)", got, retry, err)
	}
	if stats.wait != "complete" || stats.fallback != "retry" {
		t.Fatalf("request stats = %+v", stats)
	}
	if st := s.Stats(); st.Entries != 1 || st.Fills != 0 || st.Reserved != 0 || st.Evictions != 1 {
		t.Fatalf("cache stats = %+v, want one replacement and one eviction", st)
	}
}

func TestCacheFillWaitReturnsCancellationError(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("leader-aborted=%v", complete), func(t *testing.T) {
			store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			req, resp := cachePolicyFixture()
			meta, ok := objectMetadata(req, resp)
			if !ok {
				t.Fatal("invalid fixture metadata")
			}
			leader, err := store.Begin(meta)
			if err != nil {
				t.Fatal(err)
			}
			defer leader.Abort()
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			closes := 0
			resp.Body = &closeCallbackBody{ReadCloser: io.NopCloser(strings.NewReader("abc")), close: func() {
				closes++
				cancel()
				if complete {
					// Both select cases are ready. Whichever wins, cancellation
					// must be an error rather than a request for another GET.
					leader.Abort()
				}
			}}
			p := &Proxy{cache: store, metrics: newMetrics()}
			p.cfg.Cache.FillWaitTimeout = time.Minute
			stats := &requestStats{}
			got, retry, err := p.cachedResponse(req, resp, true, true, stats)
			if got != nil || retry || !errors.Is(err, context.Canceled) {
				t.Fatalf("cachedResponse = (%v, %v, %v), want (nil, false, canceled)", got, retry, err)
			}
			if closes != 1 || stats.upstreamBytes != 0 || stats.fallback != "" || p.metrics.activeWaits != 0 {
				t.Fatalf("cancellation cleanup: closes=%d stats=%+v activeWaits=%d", closes, stats, p.metrics.activeWaits)
			}
			if !complete && store.Stats().Fills != 1 {
				t.Fatal("follower cancellation disturbed leader fill")
			}
		})
	}
}

func TestShutdownCancelsCacheFillLeaderAndWaitingFollower(t *testing.T) {
	const body = "never completed cache body"
	leaderHeaders := make(chan struct{})
	followerClosed := make(chan struct{})
	leaderCanceled := make(chan struct{})
	var calls atomic.Int32
	var followerOnce, leaderOnce sync.Once
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(leaderHeaders)
			<-r.Context().Done()
			leaderOnce.Do(func() { close(leaderCanceled) })
			return
		}
		<-r.Context().Done()
		followerOnce.Do(func() { close(followerClosed) })
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.Cache.FillWaitTimeout = time.Minute

	results := make(chan error, 2)
	request := func(token string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, err := waitRequest(t, client, origin.URL+blobURL(body), token, "", ctx)
		results <- err
	}
	go request("Bearer leader")
	waitFor(t, leaderHeaders, "leader headers")
	waitForFills(t, p, 1)
	go request("Bearer follower")
	waitFor(t, followerClosed, "follower upstream body close")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := p.Shutdown(ctx)
	cancel()
	if err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v, want deadline exceeded", err)
	}
	waitFor(t, leaderCanceled, "leader origin cancellation")
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("client unexpectedly received a complete response")
			}
		case <-time.After(waitTestTimeout):
			t.Fatal("timed out waiting for client shutdown error")
		}
	}
	eventuallyLifecycle(t, time.Second, func() bool {
		st := p.cache.Stats()
		return st.Fills == 0 && st.Reserved == 0 && st.Entries == 0 && lifecycleConnCount(p) == 0
	}, "shutdown cleanup without partial cache entry")
}
