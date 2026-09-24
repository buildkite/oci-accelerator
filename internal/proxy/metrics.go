package proxy

import (
	"fmt"
	"maps"
	"net/http"
	"runtime"
	runtimemetrics "runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

type metrics struct {
	mu                             sync.Mutex
	requests                       map[string]uint64
	statuses                       map[string]uint64
	cache                          map[string]uint64
	fills                          map[string]uint64
	waits                          map[string]uint64
	fallback                       map[string]uint64
	rejects                        map[string]uint64
	upstream                       uint64
	upstreamBytes, downstreamBytes uint64
	servedBytes, servedEntries     map[string]uint64
	durations                      [len(requestDurationBounds) + 1]uint64
	durationSum                    float64
	activeRequests, activeWaits    int64
}

func newMetrics() *metrics {
	return &metrics{
		requests:      map[string]uint64{},
		statuses:      map[string]uint64{},
		cache:         map[string]uint64{},
		fills:         map[string]uint64{},
		waits:         map[string]uint64{},
		fallback:      map[string]uint64{},
		rejects:       map[string]uint64{},
		servedBytes:   map[string]uint64{"cache": 0, "origin": 0},
		servedEntries: map[string]uint64{"cache": 0, "origin": 0},
	}
}

type requestStats struct {
	cache, fill    string
	wait, fallback string
	admission      string
	reject         string
	upstreamBytes  int64
	immutable      bool // Original request is a full immutable-object GET, before header stripping.
}
type statsKey struct{}

// Finite histogram bounds, shared by classification and scrape labels.
var requestDurationBounds = [...]time.Duration{
	10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second,
}

func (p *Proxy) MetricsHandler() http.Handler { return http.HandlerFunc(p.writeMetrics) }

func (p *Proxy) writeMetrics(w http.ResponseWriter, _ *http.Request) {
	// Take consistent point-in-time copies of proxy, cache, and heap counters.
	m := p.metrics.snapshot()
	cs := cacheStats(p)
	heapAlloc := heapAllocBytes()

	// Emit the Prometheus text exposition content type.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder

	// Helper that renders a labeled counter family with deterministically sorted keys.
	writeMap := func(name, label string, values map[string]uint64) {
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, label, k, values[k])
		}
	}

	// Helper that renders a single unlabeled scalar counter or gauge.
	writeScalar := func(name, kind string, value any) {
		fmt.Fprintf(&b, "# TYPE oci_accelerator_%s %s\noci_accelerator_%s %d\n", name, kind, name, value)
	}

	// Emit all labeled counter families collected from the request pipeline.
	writeMap("oci_accelerator_requests_total", "outcome", m.requests)
	writeMap("oci_accelerator_request_status_total", "class", m.statuses)
	writeMap("oci_accelerator_cache_total", "outcome", m.cache)
	writeMap("oci_accelerator_fills_total", "outcome", m.fills)
	writeMap("oci_accelerator_waits_total", "outcome", m.waits)
	writeMap("oci_accelerator_fallbacks_total", "outcome", m.fallback)
	writeMap("oci_accelerator_cache_rejections_total", "reason", m.rejects)

	// Emit upstream/downstream traffic counters and live activity gauges.
	b.WriteString("# HELP oci_accelerator_upstream_get_attempts_total GET connection-acquisition attempts, including transport retries and DNS/dial/TLS failures before sending.\n")
	writeScalar("upstream_get_attempts_total", "counter", m.upstream)
	writeScalar("upstream_body_bytes_total", "counter", m.upstreamBytes)
	writeScalar("downstream_bytes_total", "counter", m.downstreamBytes)
	b.WriteString("# HELP oci_accelerator_served_bytes_total Immutable-object 200 response body bytes written downstream, including interrupted transfers, by body source.\n")
	writeMap("oci_accelerator_served_bytes_total", "source", m.servedBytes)
	b.WriteString("# HELP oci_accelerator_served_entries_total Completed immutable-object 200 response deliveries, not unique objects, by body source.\n")
	writeMap("oci_accelerator_served_entries_total", "source", m.servedEntries)
	writeScalar("active_requests", "gauge", m.activeRequests)
	writeScalar("active_waits", "gauge", m.activeWaits)
	writeScalar("active_connections", "gauge", p.connectionCount())

	// Emit cache occupancy, fill, and eviction statistics.
	writeScalar("cache_entries", "gauge", cs.Entries)
	writeScalar("cache_fills", "gauge", cs.Fills+cs.Pending)
	writeScalar("cache_bytes", "gauge", cs.Bytes)
	writeScalar("cache_reserved_bytes", "gauge", cs.Reserved)
	writeScalar("cache_readers", "gauge", cs.Readers)
	writeScalar("cache_evictions_total", "counter", cs.Evictions)
	writeScalar("store_fill_success_total", "counter", cs.FillSuccess)
	writeScalar("store_fill_failure_total", "counter", cs.FillFailure)
	writeScalar("store_fill_abort_total", "counter", cs.FillAbort)
	writeScalar("store_fill_expiry_total", "counter", cs.FillExpiry)

	// Emit process-level runtime gauges.
	writeScalar("heap_alloc_bytes", "gauge", heapAlloc)
	writeScalar("goroutines", "gauge", runtime.NumGoroutine())

	// Emit the per-reason cache admission counter across every enumerated reason.
	b.WriteString("# TYPE oci_accelerator_cache_admissions_total counter\n")
	for reason := cache.AdmissionReason(0); reason < cache.AdmissionReasonCount; reason++ {
		fmt.Fprintf(&b, "oci_accelerator_cache_admissions_total{reason=%q} %d\n", reason.String(), cs.Admissions[reason])
	}

	// Emit the request-duration histogram with cumulative bucket counts.
	b.WriteString("# TYPE oci_accelerator_request_duration_seconds histogram\n")
	var cumulative uint64
	for i, count := range m.durations {
		cumulative += count
		label := "+Inf"
		if i < len(requestDurationBounds) {
			label = strconv.FormatFloat(requestDurationBounds[i].Seconds(), 'g', -1, 64)
		}
		fmt.Fprintf(&b, "oci_accelerator_request_duration_seconds_bucket{le=%q} %d\n", label, cumulative)
	}

	// Emit the histogram total count and sum, then flush the response.
	fmt.Fprintf(&b, "oci_accelerator_request_duration_seconds_count %d\noci_accelerator_request_duration_seconds_sum %g\n", cumulative, m.durationSum)
	_, _ = fmt.Fprint(w, b.String())
}

func (m *metrics) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return metricsSnapshot{
		requests:        maps.Clone(m.requests),
		statuses:        maps.Clone(m.statuses),
		cache:           maps.Clone(m.cache),
		fills:           maps.Clone(m.fills),
		waits:           maps.Clone(m.waits),
		fallback:        maps.Clone(m.fallback),
		rejects:         maps.Clone(m.rejects),
		upstream:        m.upstream,
		upstreamBytes:   m.upstreamBytes,
		downstreamBytes: m.downstreamBytes,
		servedBytes:     maps.Clone(m.servedBytes),
		servedEntries:   maps.Clone(m.servedEntries),
		durations:       m.durations,
		durationSum:     m.durationSum,
		activeRequests:  m.activeRequests,
		activeWaits:     m.activeWaits,
	}
}

type metricsSnapshot struct {
	requests, statuses, cache, fills, waits, fallback, rejects map[string]uint64
	upstream, upstreamBytes, downstreamBytes                   uint64
	servedBytes, servedEntries                                 map[string]uint64
	durations                                                  [len(requestDurationBounds) + 1]uint64
	durationSum                                                float64
	activeRequests, activeWaits                                int64
}

func cacheStats(p *Proxy) cache.Stats {
	if p.cache == nil {
		return cache.Stats{}
	}
	return p.cache.Stats()
}

func (p *Proxy) connectionCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.conns) }

// heapAllocBytes returns the live heap-object bytes without stopping the world.
// Reading "/memory/classes/heap/objects:bytes" via runtime/metrics avoids the
// stop-the-world pause that runtime.ReadMemStats imposes on every scrape.
func heapAllocBytes() uint64 {
	samples := []runtimemetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	runtimemetrics.Read(samples)
	if samples[0].Value.Kind() != runtimemetrics.KindUint64 {
		return 0
	}
	return samples[0].Value.Uint64()
}

func (m *metrics) finish(s *requestStats, outcome string, status int, d time.Duration, downstream int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRequests--
	m.requests[outcome]++
	m.statuses[statusClass(status)]++
	m.cache[s.cache]++
	// Detached range fills report their outcome when their pump finishes.
	if s.fill != "none" && s.fill != "background" {
		m.fills[s.fill]++
	}
	if s.wait != "" {
		m.waits[s.wait]++
	}
	if s.fallback != "" {
		m.fallback[s.fallback]++
	}
	if s.reject != "" {
		m.rejects[s.reject]++
	}
	m.upstreamBytes += uint64(s.upstreamBytes)
	m.downstreamBytes += uint64(downstream)
	// Scope by request shape and response status, not storage admission. Origin
	// responses still count when policy or capacity prevents caching them.
	if s.immutable && status == http.StatusOK {
		source := "origin"
		if s.cache == "hit" || s.cache == "wait_hit" {
			source = "cache"
		}
		m.servedBytes[source] += uint64(downstream)
		if outcome == "complete" {
			m.servedEntries[source]++
		}
	}
	i := 0
	for i < len(requestDurationBounds) && d > requestDurationBounds[i] {
		i++
	}
	m.durations[i]++
	m.durationSum += d.Seconds()
}

func statusClass(status int) string {
	if status >= 100 && status < 600 {
		return fmt.Sprintf("%dxx", status/100)
	}
	return "other"
}
