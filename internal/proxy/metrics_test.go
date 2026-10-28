package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetricsSnapshotCopiesAllFieldsAndOwnsMaps(t *testing.T) {
	m := newMetrics()
	m.upstream = 2
	m.activeRequests = 6
	m.activeWaits = 7
	s := &requestStats{cache: "miss", fill: "stored", wait: "complete", fallback: "retry", reject: "vary", upstreamBytes: 3, immutable: true, peer: "unavailable"}
	m.finish(s, "complete", http.StatusOK, 20*time.Millisecond, 4)
	want := metricsSnapshot{
		requests:        map[string]uint64{"complete": 1},
		statuses:        map[string]uint64{"2xx": 1},
		cache:           map[string]uint64{"miss": 1},
		fills:           map[string]uint64{"stored": 1},
		waits:           map[string]uint64{"complete": 1},
		fallback:        map[string]uint64{"retry": 1},
		rejects:         map[string]uint64{"vary": 1},
		peers:           map[string]uint64{"unavailable": 1},
		upstream:        2,
		upstreamBytes:   3,
		downstreamBytes: 4,
		servedBytes:     map[string]uint64{"cache": 0, "origin": 4},
		servedEntries:   map[string]uint64{"cache": 0, "origin": 1},
		durations:       [8]uint64{0, 1},
		durationSum:     .02,
		activeRequests:  5,
		activeWaits:     7,
	}
	got := m.snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	for _, counts := range []map[string]uint64{got.requests, got.statuses, got.cache, got.fills, got.waits, got.fallback, got.rejects, got.peers, got.servedBytes, got.servedEntries} {
		for key := range counts {
			counts[key]++
		}
		counts["snapshot-only"] = 99
	}
	got.durations[1]++
	if current := m.snapshot(); !reflect.DeepEqual(current, want) {
		t.Fatalf("snapshot mutation changed live metrics: %+v", current)
	}
	previous := m.snapshot()
	m.finish(s, "complete", http.StatusOK, time.Second, 8)
	if !reflect.DeepEqual(previous, want) {
		t.Fatalf("live update changed an existing snapshot: %+v", previous)
	}
}

func TestHTTPTransferMetricsColdWarmDeniedNoStoreCorruptAndFailure(t *testing.T) {
	const good = "metric transfer body"
	for _, tc := range []struct {
		name, mode                                     string
		requests, gets, upstreamBytes, downstreamBytes uint64
		cache, fills, statuses                         map[string]uint64
		servedBytes, servedEntries                     map[string]uint64
		storeSuccess, storeFailure                     int64
	}{
		{"cold", "cold", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"miss": 1}, map[string]uint64{"stored": 1}, map[string]uint64{"2xx": 1}, map[string]uint64{"cache": 0, "origin": uint64(len(good))}, map[string]uint64{"cache": 0, "origin": 1}, 1, 0},
		{"warm", "warm", 2, 2, uint64(len(good)), uint64(2 * len(good)), map[string]uint64{"miss": 1, "hit": 1}, map[string]uint64{"stored": 1}, map[string]uint64{"2xx": 2}, map[string]uint64{"cache": uint64(len(good)), "origin": uint64(len(good))}, map[string]uint64{"cache": 1, "origin": 1}, 1, 0},
		{"denied", "denied", 1, 1, uint64(len("denied")), uint64(len("denied")), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"4xx": 1}, map[string]uint64{"cache": 0, "origin": 0}, map[string]uint64{"cache": 0, "origin": 0}, 0, 0},
		{"no-store", "no-store", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"2xx": 1}, map[string]uint64{"cache": 0, "origin": uint64(len(good))}, map[string]uint64{"cache": 0, "origin": 1}, 0, 0},
		{"corrupt-digest", "corrupt", 1, 1, uint64(len(good)), uint64(len(good)), map[string]uint64{"miss": 1}, map[string]uint64{"failed": 1}, map[string]uint64{"2xx": 1}, map[string]uint64{"cache": 0, "origin": uint64(len(good))}, map[string]uint64{"cache": 0, "origin": 1}, 0, 1},
		{"upstream-5xx", "failure", 1, 1, uint64(len("failure")), uint64(len("failure")), map[string]uint64{"bypass": 1}, map[string]uint64{}, map[string]uint64{"5xx": 1}, map[string]uint64{"cache": 0, "origin": 0}, map[string]uint64{"cache": 0, "origin": 0}, 0, 0},
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
			m, st := p.metrics.snapshot(), p.cache.Stats()
			if uint64(gets.Load()) != tc.gets || m.upstream != tc.gets || m.upstreamBytes != tc.upstreamBytes || m.downstreamBytes != tc.downstreamBytes || m.requests["complete"] != tc.requests || fmt.Sprint(m.cache) != fmt.Sprint(tc.cache) || fmt.Sprint(m.fills) != fmt.Sprint(tc.fills) || fmt.Sprint(m.statuses) != fmt.Sprint(tc.statuses) || !reflect.DeepEqual(m.servedBytes, tc.servedBytes) || !reflect.DeepEqual(m.servedEntries, tc.servedEntries) || st.FillSuccess != tc.storeSuccess || st.FillFailure != tc.storeFailure {
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
	m := p.metrics.snapshot()
	if resp.StatusCode != 502 || m.upstream != 1 || m.upstreamBytes != 0 || m.statuses["5xx"] != 1 || m.requests["complete"] != 1 {
		t.Fatalf("failed upstream metrics: %+v status=%d", m, resp.StatusCode)
	}
}

func TestMetricsCountsAutomaticTransportRetries(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var firstRemote string
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer retry-token" {
					t.Error("retry lost authorization")
				}

				n := calls.Add(1)
				mu.Lock()
				if n == 1 {
					firstRemote = r.RemoteAddr
				}
				reused := r.RemoteAddr == firstRemote
				mu.Unlock()

				if n == 2 {
					if !reused {
						t.Error("second request did not reuse the upstream connection")
						return
					}

					// The request on a reused upstream connection is received but
					// gets no response. Transport must replay it on a new one.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}

				io.WriteString(w, "retry body")
			}))
			defer origin.Close()
			p, _, client := interceptFixture(t, origin, nil, true)
			for i := 0; i < 2; i++ {
				req, _ := http.NewRequest(method, origin.URL+"/v2/", nil)
				req.Header.Set("Authorization", "Bearer retry-token")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				wantBody := "retry body"
				if method == http.MethodHead {
					wantBody = ""
				}
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != wantBody {
					t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
				}
			}

			awaitNoActiveRequests(t, p)
			wantAttempts := uint64(3)
			if method == http.MethodHead {
				wantAttempts = 0
			}
			if calls.Load() != 3 || p.metrics.snapshot().upstream != wantAttempts {
				t.Fatalf("origin calls=%d GET attempts=%d, want 3/%d", calls.Load(), p.metrics.snapshot().upstream, wantAttempts)
			}
		})
	}
}

func TestMetricsCountsRedirectsWithoutStackingTrace(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/terminal" {
			w.Header().Set("Location", "/terminal")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, "terminal body")
	}))
	defer origin.Close()
	p, _, _ := interceptFixture(t, origin, nil, true)
	var traced atomic.Int32
	req, _ := http.NewRequest(http.MethodGet, origin.URL+testBlobPath, nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(string) { traced.Add(1) },
	}))
	// Exercise the upstream transport directly to verify an existing trace is
	// preserved and invoked once per hop, not duplicated when following it.
	transport := registryTransport{upstream: p.transport, metrics: p.metrics}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "terminal body" || calls.Load() != 2 || traced.Load() != 2 || p.metrics.snapshot().upstream != 2 {
		t.Fatalf("body=%q err=%v calls=%d trace=%d attempts=%d", body, err, calls.Load(), traced.Load(), p.metrics.snapshot().upstream)
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
	for _, want := range []string{"oci_accelerator_upstream_get_attempts_total 2", `oci_accelerator_requests_total{outcome="complete"} 1`, `oci_accelerator_served_bytes_total{source="cache"} 0`, `oci_accelerator_served_bytes_total{source="origin"} 0`, `oci_accelerator_served_entries_total{source="cache"} 0`, `oci_accelerator_served_entries_total{source="origin"} 0`, "oci_accelerator_heap_alloc_bytes"} {
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

func TestImmutableObjectMetricsBlobManifestAndAuthorizationGrantHits(t *testing.T) {
	const blob = "short blob"
	const manifest = `{"schemaVersion":2,"deliberately":"longer manifest"}`
	var gets atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		body := blob
		if strings.Contains(r.URL.Path, "/manifests/") {
			body = manifest
		}
		blobHeaders(w, body)
		if body == manifest {
			w.Header().Set("Content-Type", manifestMediaTypes[0])
		}
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	p.authorization = newAuthorizationCache(time.Minute, time.Minute, 10)
	// Unequal sizes and hit counts distinguish byte share from entry share.
	for _, tc := range []struct {
		path, body string
		requests   int
	}{
		{blobURL(blob), blob, 3},
		{manifestURL("team/image", manifest), manifest, 2},
	} {
		for range tc.requests {
			resp, got := getBlob(t, client, origin.URL+tc.path, "Bearer metrics")
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || got != tc.body {
				t.Fatalf("delivery = %d %q, want 200 %q", resp.StatusCode, got, tc.body)
			}
			awaitNoActiveRequests(t, p)
		}
	}
	if gets.Load() != 2 {
		t.Fatalf("origin GETs = %d, want one per object and none for grant hits", gets.Load())
	}
	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		fmt.Sprintf(`oci_accelerator_served_bytes_total{source="cache"} %d`, 2*len(blob)+len(manifest)),
		fmt.Sprintf(`oci_accelerator_served_bytes_total{source="origin"} %d`, len(blob)+len(manifest)),
		`oci_accelerator_served_entries_total{source="cache"} 3`,
		`oci_accelerator_served_entries_total{source="origin"} 2`,
	} {
		if !strings.Contains(w.Body.String(), want+"\n") {
			t.Errorf("missing %q\n%s", want, w.Body.String())
		}
	}
}

func TestImmutableObjectMetricsExcludeBypassesAndCountInterruptedBytesOnly(t *testing.T) {
	m := newMetrics()
	m.activeRequests = 4
	for _, stats := range []*requestStats{
		{immutable: false, cache: "hit"}, // query, unsupported digest, or an original conditional/range request
		{immutable: true, cache: "miss"}, // interrupted origin delivery
		{immutable: true, cache: "hit"},  // interrupted cached delivery
	} {
		m.finish(stats, "aborted", http.StatusOK, time.Millisecond, 7)
	}
	m.finish(&requestStats{immutable: true, cache: "miss"}, "complete", http.StatusBadGateway, time.Millisecond, 7)
	got := m.snapshot()
	if want := (map[string]uint64{"cache": 7, "origin": 7}); !reflect.DeepEqual(got.servedBytes, want) {
		t.Fatalf("served bytes = %v, want %v", got.servedBytes, want)
	}
	if want := (map[string]uint64{"cache": 0, "origin": 0}); !reflect.DeepEqual(got.servedEntries, want) {
		t.Fatalf("served entries = %v, want %v", got.servedEntries, want)
	}
}

func TestImmutableObjectMetricsExcludeUnsupportedRequests(t *testing.T) {
	const body = "bypass body"
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Range") != "" || r.Header.Get("If-None-Match") != "" || r.Header.Get("Connection") != "" {
			t.Error("hop headers reached origin")
		}
		blobHeaders(w, body)
		io.WriteString(w, body)
	}))
	defer origin.Close()
	p, client := cacheFixture(t, origin, 1<<20, nil)
	for _, tc := range []struct {
		method, path, header, value string
	}{
		{http.MethodGet, blobURL(body), "Range", "bytes=0-1"},
		{http.MethodGet, blobURL(body), "If-None-Match", `"etag"`},
		{http.MethodGet, "/v2/team/image/manifests/latest", "", ""},
		{http.MethodGet, blobURL(body) + "?token=example", "", ""},
		{http.MethodHead, blobURL(body), "", ""},
	} {
		req, err := http.NewRequest(tc.method, origin.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.header != "" {
			req.Header.Set("Connection", tc.header)
			req.Header.Set(tc.header, tc.value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := body
		if tc.method == http.MethodHead {
			want = ""
		}
		if err != nil || resp.StatusCode != http.StatusOK || string(got) != want {
			t.Fatalf("bypass = %d %q %v, want 200 %q", resp.StatusCode, got, err, want)
		}
	}
	awaitNoActiveRequests(t, p)
	m := p.metrics.snapshot()
	if calls.Load() != 5 || m.servedBytes["cache"] != 0 || m.servedBytes["origin"] != 0 || m.servedEntries["cache"] != 0 || m.servedEntries["origin"] != 0 {
		t.Fatalf("unsupported request metrics: calls=%d metrics=%+v", calls.Load(), m)
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
	line := "\noci_accelerator_heap_alloc_bytes "
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
		"# TYPE oci_accelerator_request_duration_seconds histogram",
		`oci_accelerator_request_status_total{class="4xx"} 1`,
		`oci_accelerator_request_status_total{class="other"} 1`,
		`oci_accelerator_requests_total{outcome="complete"} 1`,
		`oci_accelerator_requests_total{outcome="aborted"} 1`,
		`oci_accelerator_request_duration_seconds_bucket{le="0.01"} 0`,
		`oci_accelerator_request_duration_seconds_bucket{le="0.05"} 1`,
		`oci_accelerator_request_duration_seconds_bucket{le="+Inf"} 2`,
		"oci_accelerator_request_duration_seconds_count 2",
		"oci_accelerator_request_duration_seconds_sum " + strconv.FormatFloat((20*time.Millisecond+6*time.Second).Seconds(), 'g', -1, 64),
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing exact metric %q\n%s", want, body)
		}
	}
}

func TestMetricsHistogramBoundaryObservations(t *testing.T) {
	boundaries := []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second}
	labels := []string{"0.01", "0.05", "0.1", "0.25", "0.5", "1", "5"}
	p := &Proxy{metrics: newMetrics(), conns: make(map[*trackedConn]struct{})}
	p.metrics.activeRequests = int64(3 * len(boundaries))
	for _, boundary := range boundaries {
		for _, duration := range []time.Duration{boundary - time.Nanosecond, boundary, boundary + time.Nanosecond} {
			p.metrics.finish(&requestStats{}, "complete", http.StatusOK, duration, 0)
		}
	}

	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for i, label := range labels {
		want := fmt.Sprintf("oci_accelerator_request_duration_seconds_bucket{le=%q} %d\n", label, 3*i+2)
		if !strings.Contains(body, want) {
			t.Errorf("missing histogram sample %q\n%s", strings.TrimSpace(want), body)
		}
	}
	for _, want := range []string{
		`oci_accelerator_request_duration_seconds_bucket{le="+Inf"} 21` + "\n",
		"oci_accelerator_request_duration_seconds_count 21\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing histogram sample %q\n%s", strings.TrimSpace(want), body)
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
