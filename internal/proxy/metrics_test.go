package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func awaitNoActiveRequests(t *testing.T, p *Proxy) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.metrics.mu.Lock()
		active := p.metrics.activeRequests
		p.metrics.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("metrics request completion was not recorded")
}

func metricsValues(p *Proxy) metricsSnapshot {
	p.metrics.mu.Lock()
	defer p.metrics.mu.Unlock()
	return metricsSnapshot{requests: cloneMap(p.metrics.requests), statuses: cloneMap(p.metrics.statuses), cache: cloneMap(p.metrics.cache), fills: cloneMap(p.metrics.fills), waits: cloneMap(p.metrics.waits), fallback: cloneMap(p.metrics.fallback), upstream: p.metrics.upstream, upstreamBytes: p.metrics.upstreamBytes, downstreamBytes: p.metrics.downstreamBytes}
}

func TestHTTPTransferMetricsColdWarmDeniedNoStoreCorruptAndFailure(t *testing.T) {
	const good = "metric transfer body"
	for _, tc := range []struct {
		name, mode                                     string
		requests, gets, upstreamBytes, downstreamBytes uint64
		cache, fills, statuses                         map[string]uint64
		storeSuccess, storeFailure                     int64
	}{
		{"cold", "cold", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"miss": 1}, map[string]uint64{"stored": 1}, map[string]uint64{"2xx": 1}, 1, 0},
		{"warm", "warm", 2, 2, uint64(len(good)), uint64(2 * len(good)), map[string]uint64{"miss": 1, "hit": 1}, map[string]uint64{"stored": 1}, map[string]uint64{"2xx": 2}, 1, 0},
		{"denied", "denied", 1, 1, uint64(len("denied")), uint64(len("denied")), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"4xx": 1}, 0, 0},
		{"no-store", "no-store", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"2xx": 1}, 0, 0},
		{"corrupt-digest", "corrupt", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"miss": 1}, map[string]uint64{"failed": 1}, map[string]uint64{"2xx": 1}, 0, 1},
		{"upstream-5xx", "failure", 1, 1, uint64(len("failure")), uint64(len("failure")), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"5xx": 1}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := gets.Add(1)
				if tc.mode == "denied" {
					w.WriteHeader(401)
					io.WriteString(w, "denied")
					return
				}
				if tc.mode == "failure" {
					w.WriteHeader(503)
					io.WriteString(w, "failure")
					return
				}
				blobHeaders(w, good)
				if tc.mode == "no-store" {
					w.Header().Set("Cache-Control", "no-store")
				}
				if tc.mode == "warm" && n == 2 {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				body := good
				if tc.mode == "corrupt" {
					body = strings.Repeat("x", len(good))
				}
				io.WriteString(w, body)
			}))
			defer origin.Close()
			p, c := cacheFixture(t, origin, 1<<20, nil)
			count := 1
			if tc.mode == "warm" {
				count = 2
			}
			for i := 0; i < count; i++ {
				resp, _ := getBlob(t, c, origin.URL+blobURL(good), "Bearer secret")
				resp.Body.Close()
				awaitNoActiveRequests(t, p)
			}
			m, st := metricsValues(p), p.cache.Stats()
			if uint64(gets.Load()) != tc.gets || m.upstream != tc.gets || m.upstreamBytes != tc.upstreamBytes || m.downstreamBytes != tc.downstreamBytes || m.requests["complete"] != tc.requests || fmt.Sprint(m.cache) != fmt.Sprint(tc.cache) || fmt.Sprint(m.fills) != fmt.Sprint(tc.fills) || fmt.Sprint(m.statuses) != fmt.Sprint(tc.statuses) || st.FillSuccess != tc.storeSuccess || st.FillFailure != tc.storeFailure {
				t.Fatalf("gets=%d metrics=%+v store=%+v", gets.Load(), m, st)
			}
		})
	}
}

func TestMetricsScrapeConcurrentRequestCancellationAndShutdownIsRedacted(t *testing.T) {
	const secret = "Bearer scrape-secret"
	started := make(chan struct{})
	var once sync.Once
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer origin.Close()
	p, c := cacheFixture(t, origin, 1<<20, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = waitRequest(t, c, origin.URL+blobURL("private-digest-body"), secret, "", ctx)
	}()
	waitFor(t, started, "upstream request")
	shutdownDone := make(chan error, 1)
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		p.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/private/path/sha256:deadbeef?token=secret", nil))
		for _, forbidden := range []string{"scrape-secret", "private/path", "deadbeef"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatalf("scrape leaked %q", forbidden)
			}
		}
		if i == 20 {
			cancel()
		}
		if i == 40 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				shutdownDone <- p.Shutdown(ctx)
			}()
		}
	}
	<-done
	awaitNoActiveRequests(t, p)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
}

func TestMetricsCountsFailedTransportGET(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	p, c := cacheFixture(t, origin, 1<<20, nil)
	origin.Close()
	resp, _ := getBlob(t, c, origin.URL+blobURL("unreachable"), "Bearer secret")
	awaitNoActiveRequests(t, p)
	m := metricsValues(p)
	if resp.StatusCode != 502 || m.upstream != 1 || m.upstreamBytes != 0 || m.statuses["5xx"] != 1 || m.requests["complete"] != 1 {
		t.Fatalf("failed upstream metrics: %+v status=%d", m, resp.StatusCode)
	}
}

func TestMetricsHandlerIsBoundedAndRedacted(t *testing.T) {
	p := &Proxy{metrics: newMetrics(), conns: make(map[*trackedConn]struct{})}
	p.metrics.upstream = 2
	p.metrics.requests["complete"] = 1
	r := httptest.NewRequest("GET", "http://metrics.invalid/metrics?token=secret", nil)
	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{"docker_accelerator_upstream_get_attempts_total 2", `docker_accelerator_requests_total{outcome="complete"} 1`, "docker_accelerator_heap_alloc_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	for _, forbidden := range []string{"secret", r.URL.Path, r.URL.RawQuery} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics leaked %q", forbidden)
		}
	}
}

func TestHeapAllocBytesGaugeReportsPositiveLiveHeap(t *testing.T) {
	if got := heapAllocBytes(); got == 0 {
		t.Fatal("heapAllocBytes() = 0, want live heap-object bytes > 0")
	}
	p := &Proxy{metrics: newMetrics(), conns: make(map[*trackedConn]struct{})}
	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	var value uint64
	// Match the sample line, not the "# TYPE ... gauge" declaration, by anchoring
	// on the preceding newline.
	line := "\ndocker_accelerator_heap_alloc_bytes "
	idx := strings.Index(body, line)
	if idx < 0 {
		t.Fatalf("heap_alloc_bytes gauge missing:\n%s", body)
	}
	rest := body[idx+len(line):]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		t.Fatalf("heap_alloc_bytes gauge not newline-terminated: %q", rest)
	}
	if _, err := fmt.Sscanf(rest[:end], "%d", &value); err != nil || value == 0 {
		t.Fatalf("heap_alloc_bytes value = %q (parsed %d, err %v), want positive integer", rest[:end], value, err)
	}
}

func TestMetricsHistogramStatusAndOutcomeCounts(t *testing.T) {
	p := &Proxy{metrics: newMetrics(), conns: make(map[*trackedConn]struct{})}
	p.metrics.activeRequests = 2
	p.metrics.finish(&requestStats{cache: "miss", fill: "stored"}, "complete", 404, 20*time.Millisecond, 12)
	p.metrics.finish(&requestStats{cache: "bypass", fill: "none"}, "aborted", 0, 6*time.Second, 3)
	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE docker_accelerator_request_duration_seconds histogram",
		`docker_accelerator_request_status_total{class="4xx"} 1`,
		`docker_accelerator_request_status_total{class="other"} 1`,
		`docker_accelerator_requests_total{outcome="complete"} 1`,
		`docker_accelerator_requests_total{outcome="aborted"} 1`,
		`docker_accelerator_request_duration_seconds_bucket{le="0.01"} 0`,
		`docker_accelerator_request_duration_seconds_bucket{le="0.05"} 1`,
		`docker_accelerator_request_duration_seconds_bucket{le="+Inf"} 2`,
		"docker_accelerator_request_duration_seconds_count 2",
		"docker_accelerator_request_duration_seconds_sum " + strconv.FormatFloat((20*time.Millisecond+6*time.Second).Seconds(), 'g', -1, 64),
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing exact metric %q\n%s", want, body)
		}
	}
}

func TestStatusClassIsBounded(t *testing.T) {
	for status, want := range map[int]string{99: "other", 100: "1xx", 299: "2xx", 399: "3xx", 499: "4xx", 599: "5xx", 600: "other"} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}
