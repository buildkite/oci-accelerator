package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestColdRangeReturnsBeforeTailAndCompletesFill(t *testing.T) {
	const body = "0123456789abcdef"
	tail := make(chan struct{})
	releaseTail := safeRelease(tail)
	defer releaseTail()
	var gets, upstreamBytes atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("upstream Range = %q, want empty", got)
		}
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		n, _ := io.WriteString(w, body[:4])
		upstreamBytes.Add(int32(n))
		w.(http.Flusher).Flush()
		<-tail
		n, _ = io.WriteString(w, body[4:])
		upstreamBytes.Add(int32(n))
	}))
	defer func() { releaseTail(); origin.Close() }()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.IdleTimeout = time.Second

	// The request must finish while the origin is still withholding its tail.
	resp, got := requestRange(t, client, origin.URL+blobURL(body), "range", "bytes=1-3")
	if resp.StatusCode != http.StatusPartialContent || got != "123" {
		t.Fatalf("cold range = %d %q", resp.StatusCode, got)
	}
	if upstreamBytes.Load() != 4 {
		t.Fatalf("upstream bytes before release = %d, want 4", upstreamBytes.Load())
	}
	releaseTail()
	waitForFills(t, p, 0)
	resp, warm := requestRange(t, client, origin.URL+blobURL(body), "range", "bytes=10-15")
	if resp.StatusCode != http.StatusPartialContent || warm != body[10:] {
		t.Fatalf("warm range = %d %q", resp.StatusCode, warm)
	}
	shutdown(t, p)
	if gets.Load() != 2 || p.metrics.snapshot().upstreamBytes != uint64(len(body)) {
		t.Fatalf("upstream GETs=%d read bytes=%d, want 2 and %d", gets.Load(), p.metrics.snapshot().upstreamBytes, len(body))
	}
	if stats := p.cache.Stats(); stats.Entries != 1 || stats.Fills != 0 {
		t.Fatalf("cache stats after fill: %+v", stats)
	}
}

func TestColdMultiRangeFallsBackToFullResponse(t *testing.T) {
	const body = "cold multipart fallback"
	var gets, upstreamBytes atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Header.Get("Range") != "" {
			t.Error("Range forwarded upstream")
		}
		blobHeaders(w, body)
		n, _ := io.WriteString(w, body)
		upstreamBytes.Add(int32(n))
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)

	resp, got := requestRange(t, client, origin.URL+blobURL(body), "range", "bytes=0-2,8-10")
	if resp.StatusCode != http.StatusOK || got != body {
		t.Fatalf("cold multi-range = %d %q", resp.StatusCode, got)
	}
	waitForFills(t, p, 0)
	if gets.Load() != 1 || upstreamBytes.Load() != int32(len(body)) || p.cache.Stats().Entries != 1 {
		t.Fatalf("GETs=%d bytes=%d cache=%+v", gets.Load(), upstreamBytes.Load(), p.cache.Stats())
	}
}

func TestDisconnectedColdRangeStillFills(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"during prefix", "bytes=12-13"},
		{"mid body", "bytes=0-15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = "0123456789abcdefghijklmnop"
			gate := make(chan struct{})
			releaseGate := safeRelease(gate)
			defer releaseGate()
			var gets, upstreamBytes atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, body)
				w.WriteHeader(http.StatusOK)
				n, _ := io.WriteString(w, body[:4])
				upstreamBytes.Add(int32(n))
				w.(http.Flusher).Flush()
				<-gate
				n, _ = io.WriteString(w, body[4:])
				upstreamBytes.Add(int32(n))
			}))
			defer func() { releaseGate(); origin.Close() }()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			p.cfg.IdleTimeout = time.Second
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+blobURL(body), nil)
			req.Header.Set("Range", tc.value)
			requestDone := make(chan struct{})
			ready := make(chan struct{})
			go func() {
				resp, err := client.Do(req)
				if err == nil {
					if tc.name == "mid body" {
						_, _ = io.CopyN(io.Discard, resp.Body, 1)
					}
					close(ready)
					_, _ = io.CopyN(io.Discard, resp.Body, 1)
					_ = resp.Body.Close()
				}
				close(requestDone)
			}()
			waitFor(t, ready, "range delivery before cancellation")
			cancel()
			waitFor(t, requestDone, "canceled range request")
			releaseGate()
			waitForFills(t, p, 0)
			_, warm := getBlob(t, client, origin.URL+blobURL(body), "warm")
			shutdown(t, p)
			if warm != body || gets.Load() != 2 || p.metrics.snapshot().upstreamBytes != uint64(len(body)) {
				t.Fatalf("warm=%q GETs=%d read bytes=%d", warm, gets.Load(), p.metrics.snapshot().upstreamBytes)
			}
			if stats := p.cache.Stats(); stats.Entries != 1 || stats.Fills != 0 {
				t.Fatalf("cache stats: %+v", stats)
			}
		})
	}
}

func TestColdRangeStopsOnDeadlineShutdownOrAdmissionRefusal(t *testing.T) {
	for _, mode := range []string{"deadline", "shutdown", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			const body = "0123456789abcdef"
			closed := make(chan struct{})
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, body)
				io.WriteString(w, body[:4])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			}))
			defer origin.Close()
			limit := int64(1 << 20)
			if mode == "capacity" {
				limit = 1
			}
			p, client := cacheFixture(t, origin, limit, nil)
			p.cfg.IdleTimeout = 5 * time.Second
			if mode == "deadline" {
				p.cfg.Cache.FillTimeout = time.Second
			}
			resp, got := requestRange(t, client, origin.URL+blobURL(body), "", "bytes=1-3")
			if resp.StatusCode != 206 || got != "123" {
				t.Fatalf("selected bytes = %d %q", resp.StatusCode, got)
			}
			if mode == "shutdown" {
				shutdown(t, p)
			}
			waitFor(t, closed, "bounded full upstream GET")
			shutdown(t, p)
			if s := p.cache.Stats(); s.Entries != 0 || s.Fills != 0 || s.Reserved != 0 || s.Readers != 0 {
				t.Fatalf("resources or partial entry retained: %+v", s)
			}
			if gets.Load() != 1 || p.metrics.snapshot().upstreamBytes != 4 {
				t.Fatalf("GETs=%d bytes=%d", gets.Load(), p.metrics.snapshot().upstreamBytes)
			}
		})
	}
}

func TestColdRangeFailureNeverPublishes(t *testing.T) {
	for _, mode := range []string{"truncated", "digest"} {
		t.Run(mode, func(t *testing.T) {
			const body = "0123456789abcdef"
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				blobHeaders(w, body)
				if mode == "truncated" {
					io.WriteString(w, body[:6])
				} else {
					io.WriteString(w, "0123456789abcdeX")
				}
			}))
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			resp, got := requestRange(t, client, origin.URL+blobURL(body), "", "bytes=1-3")
			if resp.StatusCode != 206 || got != "123" {
				t.Fatalf("valid selected bytes lost: %d %q", resp.StatusCode, got)
			}
			waitForFills(t, p, 0)
			shutdown(t, p)
			if s := p.cache.Stats(); s.Entries != 0 || s.Reserved != 0 || s.FillSuccess != 0 {
				t.Fatalf("invalid body published: %+v", s)
			}
			if gets.Load() != 1 {
				t.Fatalf("unexpected retry GETs=%d", gets.Load())
			}
		})
	}
}

func TestColdRangesShareOneFullFill(t *testing.T) {
	const body = "0123456789abcdef"
	gate := make(chan struct{})
	release := safeRelease(gate)
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		blobHeaders(w, body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			io.WriteString(w, body[:4])
			w.(http.Flusher).Flush()
			<-gate
			io.WriteString(w, body[4:])
		} else {
			<-r.Context().Done()
		}
	}))
	defer func() { release(); origin.Close() }()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.cfg.IdleTimeout = 5 * time.Second
	p.cfg.Cache.FillWaitTimeout = time.Second
	url := origin.URL + blobURL(body)
	requestRange(t, client, url, "leader", "bytes=1-3")
	type result struct {
		body   string
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Range", "bytes=-3")
		req.Header.Set("Authorization", "follower")
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{body: string(b), status: resp.StatusCode, err: err}
	}()
	eventuallyLifecycle(t, time.Second, func() bool { return p.metrics.snapshot().activeWaits == 1 }, "authorized range follower")
	release()
	r := <-done
	if r.err != nil || r.status != 206 || r.body != "def" {
		t.Fatalf("follower=%+v", r)
	}
	shutdown(t, p)
	s, m := p.cache.Stats(), p.metrics.snapshot()
	if gets.Load() != 2 || s.FillSuccess != 1 || m.upstreamBytes != uint64(len(body)) || m.waits["complete"] != 1 || m.fills["stored"] != 1 {
		t.Fatalf("GETs=%d store=%+v metrics=%+v", gets.Load(), s, m)
	}
}
