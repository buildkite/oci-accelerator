package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
	"github.com/buildkite/oci-accelerator/internal/cache/cachetest"
	"github.com/buildkite/oci-accelerator/internal/config"
)

func TestPeerFillStoresAndMetersOnlyPeerBytes(t *testing.T) {
	const payload = "peer-sourced object bytes"
	transport, request, gets := peerProxyFixture(t, payload, 200)
	calls := 0
	var meta cache.Metadata
	transport.peerFetch = func(ctx context.Context, m cache.Metadata) (io.ReadCloser, error) {
		calls++
		meta = m
		if gets.Load() != 1 || transport.cache.Stats().Fills != 1 {
			t.Fatal("fetch before authorization or admission")
		}
		return io.NopCloser(strings.NewReader(payload)), nil
	}
	stats := &requestStats{immutable: true}
	request = request.WithContext(context.WithValue(request.Context(), statsKey{}, stats))
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(got) != payload || response.Header.Get("X-Origin-Approval") != "kept" {
		t.Fatalf("body=%q err=%v", got, err)
	}
	assertPeerCached(t, transport.cache, meta, payload)
	transport.metrics.finish(stats, "complete", 200, time.Millisecond, int64(len(got)))
	m := transport.metrics.snapshot()
	if m.upstream != 1 || m.upstreamBytes != 0 || m.peerBytes != uint64(len(payload)) || m.servedEntries["peer"] != 1 || m.servedBytes["peer"] != uint64(len(payload)) {
		t.Fatalf("metrics=%+v", m)
	}
	// A second delivery still authorizes upstream but must use the local copy.
	response, err = transport.RoundTrip(request.Clone(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(got) != payload || calls != 1 || gets.Load() != 2 {
		t.Fatalf("body=%q err=%v peers=%d gets=%d", got, err, calls, gets.Load())
	}
}

func TestPeerFillNeverBypassesAuthorizationOrPolicy(t *testing.T) {
	for _, scenario := range []string{"denied", "no-store", "conditional", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			status := 200
			if scenario == "denied" {
				status = 403
			}
			transport, request, gets := peerProxyFixture(t, "origin-only", status)
			if scenario == "no-store" {
				request.Header.Set("Cache-Control", "no-store")
			}
			if scenario == "conditional" {
				request.Header.Set("If-None-Match", `"old"`)
			}
			if scenario == "admission" {
				transport.cache.Close()
			}
			transport.peerFetch = func(context.Context, cache.Metadata) (io.ReadCloser, error) {
				t.Error("peer contacted without cache permission/admission")
				return nil, errors.New("unexpected")
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != status || string(got) != "origin-only" || gets.Load() != 1 {
				t.Fatalf("status=%d body=%q err=%v gets=%d", response.StatusCode, got, err, gets.Load())
			}
		})
	}
}

func TestPeerFailureFallsBackOnlyBeforeSelection(t *testing.T) {
	const payload = "full verified payload"
	for _, scenario := range []string{"unavailable", "interrupted", "corrupt", "cache-write-failed"} {
		t.Run(scenario, func(t *testing.T) {
			transport, request, gets := peerProxyFixture(t, payload, 200)
			changes := transport.cache.Changes()
			var meta cache.Metadata
			transport.peerFetch = func(_ context.Context, m cache.Metadata) (io.ReadCloser, error) {
				meta = m
				switch scenario {
				case "unavailable":
					return nil, errors.New("stale hint")
				case "interrupted":
					return &scriptedReader{steps: []readStep{{data: []byte("full ")}, {err: io.ErrUnexpectedEOF}}}, nil
				case "corrupt":
					return io.NopCloser(strings.NewReader(strings.Repeat("x", len(payload)))), nil
				case "cache-write-failed":
					// End the admitted fill before the next write, as expiry or
					// storage failure would. Delivery must still use peer bytes.
					transport.cache.Close()
				}
				return io.NopCloser(strings.NewReader(payload)), nil
			}
			stats := &requestStats{}
			request = request.WithContext(context.WithValue(request.Context(), statsKey{}, stats))
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if gets.Load() != 1 {
				t.Fatalf("retried origin: %d", gets.Load())
			}
			if scenario == "unavailable" {
				if err != nil || string(got) != payload || stats.peerBytes != 0 || stats.upstreamBytes != int64(len(payload)) {
					t.Fatalf("fallback=%q err=%v stats=%+v", got, err, stats)
				}
				assertPeerCached(t, transport.cache, meta, payload)
				return
			}
			if stats.upstreamBytes != 0 || transport.cache.Stats().Entries != 0 || transport.cache.Stats().Fills != 0 {
				t.Fatalf("origin read or partial published: stats=%+v store=%+v", stats, transport.cache.Stats())
			}
			if scenario == "interrupted" && (err != io.ErrUnexpectedEOF || string(got) != "full ") {
				t.Fatalf("interrupted=%q err=%v", got, err)
			}
			if scenario == "cache-write-failed" && (err != nil || string(got) != payload) {
				t.Fatalf("storage failure changed stream=%q err=%v", got, err)
			}
			select {
			case <-changes:
				t.Fatal("failed fill announced availability")
			default:
			}
		})
	}
}

func TestPeerFillCoalescesAfterIndependentAuthorization(t *testing.T) {
	const payload = "shared peer bytes"
	transport, request, gets := peerProxyFixture(t, payload, 200)
	transport.fillWaitTimeout = time.Second
	var peers atomic.Int32
	transport.peerFetch = func(context.Context, cache.Metadata) (io.ReadCloser, error) {
		peers.Add(1)
		return io.NopCloser(strings.NewReader(payload)), nil
	}
	leader, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Body.Close()
	follower := make(chan *http.Response, 1)
	failure := make(chan error, 1)
	go func() {
		response, err := transport.RoundTrip(request.Clone(context.Background()))
		if err != nil {
			failure <- err
		} else {
			follower <- response
		}
	}()
	eventuallyLifecycle(t, time.Second, func() bool { return transport.metrics.snapshot().activeWaits == 1 }, "peer fill follower")
	if _, err := io.Copy(io.Discard, leader.Body); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failure:
		t.Fatal(err)
	case response := <-follower:
		got, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(got) != payload || peers.Load() != 1 || gets.Load() != 2 {
			t.Fatalf("body=%q err=%v peers=%d gets=%d", got, err, peers.Load(), gets.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower stuck")
	}
}

func TestPeerRangeFillContinuesAfterClientCompletion(t *testing.T) {
	const payload = "0123456789abcdefghijklmnop"
	transport, request, gets := peerProxyFixture(t, payload, 200)
	p := New(config.Config{Cache: config.Cache{FillTimeout: time.Minute}}, nil, nil, transport.cache)
	defer p.cancelRanges()
	transport.startRangeFetch = p.startRangeFetch
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	request = request.WithContext(context.WithValue(caller, rangeRequestKey{}, true))
	request.Header.Set("Range", "bytes=3-7")
	reader, writer := io.Pipe()
	defer writer.Close()
	var meta cache.Metadata
	transport.peerFetch = func(_ context.Context, m cache.Metadata) (io.ReadCloser, error) { meta = m; return reader, nil }
	go func() { _, _ = io.WriteString(writer, payload[:8]) }()
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	cancel()
	if err != nil || response.StatusCode != 206 || string(got) != "34567" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, got, err)
	}
	if _, err := io.WriteString(writer, payload[8:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	p.inflight.Wait()
	assertPeerCached(t, transport.cache, meta, payload)
	m := transport.metrics.snapshot()
	if gets.Load() != 1 || m.peerBytes != uint64(len(payload)) || m.upstreamBytes != 0 || m.fills["stored"] != 1 {
		t.Fatalf("gets=%d metrics=%+v", gets.Load(), m)
	}
}

func TestPeerSetupCancellationAbortsFill(t *testing.T) {
	transport, request, _ := peerProxyFixture(t, "cancelled", 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport.peerFetch = func(context.Context, cache.Metadata) (io.ReadCloser, error) { cancel(); return nil, ctx.Err() }
	response, err := transport.RoundTrip(request.WithContext(ctx))
	if response != nil || err != context.Canceled || transport.cache.Stats().Fills != 0 {
		t.Fatalf("response=%v error=%v stats=%+v", response, err, transport.cache.Stats())
	}
}

func peerProxyFixture(t *testing.T, payload string, status int) (registryTransport, *http.Request, *atomic.Int32) {
	t.Helper()
	gets := &atomic.Int32{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer permitted" || r.Header.Get("Range") != "" {
			t.Errorf("upstream request not authorized full GET")
		}
		blobHeaders(w, payload)
		w.Header().Set("X-Origin-Approval", "kept")
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	t.Cleanup(origin.Close)
	store, err := cachetest.Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	request, _ := http.NewRequest("GET", origin.URL+blobURL(payload), nil)
	request.Header.Set("Authorization", "Bearer permitted")
	return registryTransport{upstream: origin.Client().Transport.(*http.Transport), cache: store, metrics: newMetrics()}, request, gets
}

func assertPeerCached(t *testing.T, store *cache.Store, meta cache.Metadata, want string) {
	t.Helper()
	reader, err := store.Get(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != want {
		t.Fatalf("cache=%q err=%v", got, err)
	}
}
