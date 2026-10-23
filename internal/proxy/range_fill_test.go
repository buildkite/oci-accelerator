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
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
			const body = "0123456789abcdef"
			tail := make(chan struct{})
			releaseTail := safeRelease(tail)
			defer releaseTail()
			var gets, upstreamBytes atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blobHeaders(w, body)
				w.WriteHeader(http.StatusOK)
				n, _ := io.WriteString(w, body[:4])
				upstreamBytes.Add(int32(n))
				w.(http.Flusher).Flush()
				<-tail
				n, _ = io.WriteString(w, body[4:])
				upstreamBytes.Add(int32(n))
			})
			origin := rangeOrigin(t, source, body, &gets, handler)
			defer func() { releaseTail(); origin.Close() }()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			peerRequests := rangePeer(t, source, p, origin.URL+blobURL(body), body, handler)
			p.cfg.IdleTimeout = time.Second

			// The request must finish while its source is still withholding its tail.
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
			if gets.Load() != 2 {
				t.Fatalf("origin GETs=%d, want 2", gets.Load())
			}
			assertRangeSourceBytes(t, p, source, peerRequests, uint64(len(body)))
			if stats := p.cache.Stats(); stats.Entries != 1 || stats.Fills != 0 {
				t.Fatalf("cache stats after fill: %+v", stats)
			}
		})
	}
}

func TestColdMultiRangeFallsBackToFullResponse(t *testing.T) {
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
			const body = "cold multipart fallback"
			var gets, upstreamBytes atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blobHeaders(w, body)
				n, _ := io.WriteString(w, body)
				upstreamBytes.Add(int32(n))
			})
			origin := rangeOrigin(t, source, body, &gets, handler)
			defer origin.Close()
			p, client := cacheFixture(t, origin, 1<<20, nil)
			peerRequests := rangePeer(t, source, p, origin.URL+blobURL(body), body, handler)

			resp, got := requestRange(t, client, origin.URL+blobURL(body), "range", "bytes=0-2,8-10")
			if resp.StatusCode != http.StatusOK || got != body {
				t.Fatalf("cold multi-range = %d %q", resp.StatusCode, got)
			}
			waitForFills(t, p, 0)
			if gets.Load() != 1 || upstreamBytes.Load() != int32(len(body)) || p.cache.Stats().Entries != 1 {
				t.Fatalf("GETs=%d bytes=%d cache=%+v", gets.Load(), upstreamBytes.Load(), p.cache.Stats())
			}
			shutdown(t, p)
			assertRangeSourceBytes(t, p, source, peerRequests, uint64(len(body)))
		})
	}
}

func TestDisconnectedColdRangeStillFills(t *testing.T) {
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
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
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						blobHeaders(w, body)
						w.WriteHeader(http.StatusOK)
						n, _ := io.WriteString(w, body[:4])
						upstreamBytes.Add(int32(n))
						w.(http.Flusher).Flush()
						<-gate
						n, _ = io.WriteString(w, body[4:])
						upstreamBytes.Add(int32(n))
					})
					origin := rangeOrigin(t, source, body, &gets, handler)
					defer func() { releaseGate(); origin.Close() }()
					p, client := cacheFixture(t, origin, 1<<20, nil)
					peerRequests := rangePeer(t, source, p, origin.URL+blobURL(body), body, handler)
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
					if warm != body || gets.Load() != 2 {
						t.Fatalf("warm=%q GETs=%d", warm, gets.Load())
					}
					assertRangeSourceBytes(t, p, source, peerRequests, uint64(len(body)))
					if stats := p.cache.Stats(); stats.Entries != 1 || stats.Fills != 0 {
						t.Fatalf("cache stats: %+v", stats)
					}
				})
			}
		})
	}
}

func TestColdRangeStopsOnDeadlineShutdownOrAdmissionRefusal(t *testing.T) {
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
			for _, mode := range []string{"deadline", "shutdown", "capacity"} {
				t.Run(mode, func(t *testing.T) {
					const body = "0123456789abcdef"
					closed := make(chan struct{})
					var gets atomic.Int32
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						blobHeaders(w, body)
						io.WriteString(w, body[:4])
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						close(closed)
					})
					origin := rangeOrigin(t, source, body, &gets, handler)
					defer origin.Close()
					limit := int64(1 << 20)
					if mode == "capacity" {
						limit = 1
					}
					p, client := cacheFixture(t, origin, limit, nil)
					peerRequests := rangePeer(t, source, p, origin.URL+blobURL(body), body, handler)
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
					if gets.Load() != 1 {
						t.Fatalf("GETs=%d, want 1", gets.Load())
					}
					assertRangeSourceBytes(t, p, source, peerRequests, 4)
				})
			}
		})
	}
}

func TestColdRangeFailureNeverPublishes(t *testing.T) {
	for _, source := range []string{"origin", "peer"} {
		t.Run(source, func(t *testing.T) {
			for _, mode := range []string{"truncated", "digest"} {
				t.Run(mode, func(t *testing.T) {
					const body = "0123456789abcdef"
					tail := make(chan struct{})
					releaseTail := safeRelease(tail)
					defer releaseTail()
					var gets atomic.Int32
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						blobHeaders(w, body)
						io.WriteString(w, body[:4])
						w.(http.Flusher).Flush()
						<-tail
						if mode == "truncated" {
							io.WriteString(w, body[4:6])
						} else {
							io.WriteString(w, "456789abcdeX")
						}
					})
					origin := rangeOrigin(t, source, body, &gets, handler)
					defer func() { releaseTail(); origin.Close() }()
					p, client := cacheFixture(t, origin, 1<<20, nil)
					peerRequests := rangePeer(t, source, p, origin.URL+blobURL(body), body, handler)
					resp, got := requestRange(t, client, origin.URL+blobURL(body), "", "bytes=1-3")
					if resp.StatusCode != 206 || got != "123" {
						t.Fatalf("valid selected bytes lost: %d %q", resp.StatusCode, got)
					}
					releaseTail()
					waitForFills(t, p, 0)
					shutdown(t, p)
					if s := p.cache.Stats(); s.Entries != 0 || s.Reserved != 0 || s.FillSuccess != 0 {
						t.Fatalf("invalid body published: %+v", s)
					}
					if gets.Load() != 1 {
						t.Fatalf("unexpected retry GETs=%d", gets.Load())
					}
					wantBytes := uint64(len(body))
					if mode == "truncated" {
						wantBytes = 6
					} else if source == "peer" {
						// The peer verifier withholds the read that fails its digest check.
						wantBytes = 4
					}
					assertRangeSourceBytes(t, p, source, peerRequests, wantBytes)
				})
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

// rangeOrigin requires a full approval GET even when a peer supplies the body.
// Withholding the approval body also makes accidental origin reads observable.
func rangeOrigin(t *testing.T, source, body string, gets *atomic.Int32, handler http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
			t.Errorf("origin approval = %s Range %q, want full GET", r.Method, r.Header.Get("Range"))
		}
		if source == "peer" {
			blobHeaders(w, body)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

func rangePeer(t *testing.T, source string, p *Proxy, url, body string, handler http.Handler) *atomic.Int32 {
	t.Helper()
	if source == "origin" {
		return new(atomic.Int32)
	}
	peer := newPeerEntryFixture(t, p, url, body, "application/octet-stream", func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("OCI-Accelerator-Node-ID", "000000000001")
			handler.ServeHTTP(w, r)
		})
	})
	p.SetPeerFetcher(peer.fetch)
	return &peer.requests
}

func assertRangeSourceBytes(t *testing.T, p *Proxy, source string, peerRequests *atomic.Int32, bytes uint64) {
	t.Helper()
	wantOrigin, wantPeer, wantRequests := bytes, uint64(0), int32(0)
	if source == "peer" {
		wantOrigin, wantPeer, wantRequests = 0, bytes, 1
	}
	m := p.metrics.snapshot()
	if m.upstreamBytes != wantOrigin || m.peerBytes != wantPeer || peerRequests.Load() != wantRequests {
		t.Fatalf("origin bytes=%d peer bytes=%d peer requests=%d, want %d %d %d", m.upstreamBytes, m.peerBytes, peerRequests.Load(), wantOrigin, wantPeer, wantRequests)
	}
}
