package proxy

import (
	"fmt"
	"net/http"
	"runtime"
	runtimemetrics "runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

// heapObjectsMetric is the live heap-object byte count. Reading it via
// runtime/metrics avoids the stop-the-world pause that runtime.ReadMemStats
// imposes on every scrape.
const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

// Finite histogram bounds, shared by classification and scrape labels.
var requestDurationBounds = [...]time.Duration{
	10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second,
}

// heapAllocBytes returns the live heap-object bytes without stopping the world.
func heapAllocBytes() uint64 {
	samples := []runtimemetrics.Sample{{Name: heapObjectsMetric}}
	runtimemetrics.Read(samples)
	if samples[0].Value.Kind() != runtimemetrics.KindUint64 {
		return 0
	}
	return samples[0].Value.Uint64()
}

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
	durations                      [len(requestDurationBounds) + 1]uint64
	durationSum                    float64
	activeRequests, activeWaits    int64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]uint64{}, statuses: map[string]uint64{}, cache: map[string]uint64{}, fills: map[string]uint64{}, waits: map[string]uint64{}, fallback: map[string]uint64{}, rejects: map[string]uint64{}}
}

type metricsSnapshot struct {
	requests, statuses, cache, fills, waits, fallback, rejects map[string]uint64
	upstream, upstreamBytes, downstreamBytes                   uint64
	durations                                                  [len(requestDurationBounds) + 1]uint64
	durationSum                                                float64
	activeRequests, activeWaits                                int64
}

func (m *metrics) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return metricsSnapshot{
		requests:        cloneMap(m.requests),
		statuses:        cloneMap(m.statuses),
		cache:           cloneMap(m.cache),
		fills:           cloneMap(m.fills),
		waits:           cloneMap(m.waits),
		fallback:        cloneMap(m.fallback),
		rejects:         cloneMap(m.rejects),
		upstream:        m.upstream,
		upstreamBytes:   m.upstreamBytes,
		downstreamBytes: m.downstreamBytes,
		durations:       m.durations,
		durationSum:     m.durationSum,
		activeRequests:  m.activeRequests,
		activeWaits:     m.activeWaits,
	}
}

func (p *Proxy) MetricsHandler() http.Handler { return http.HandlerFunc(p.writeMetrics) }

func (p *Proxy) writeMetrics(w http.ResponseWriter, _ *http.Request) {
	m := p.metrics.snapshot()
	cs := cacheStats(p)
	heapAlloc := heapAllocBytes()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
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
	writeMap("docker_accelerator_requests_total", "outcome", m.requests)
	writeMap("docker_accelerator_request_status_total", "class", m.statuses)
	writeMap("docker_accelerator_cache_total", "outcome", m.cache)
	writeMap("docker_accelerator_fills_total", "outcome", m.fills)
	writeMap("docker_accelerator_waits_total", "outcome", m.waits)
	writeMap("docker_accelerator_fallbacks_total", "outcome", m.fallback)
	writeMap("docker_accelerator_cache_rejections_total", "reason", m.rejects)
	writeScalar := func(name, kind string, value any) {
		fmt.Fprintf(&b, "# TYPE docker_accelerator_%s %s\ndocker_accelerator_%s %d\n", name, kind, name, value)
	}
	writeScalar("upstream_get_attempts_total", "counter", m.upstream)
	writeScalar("upstream_body_bytes_total", "counter", m.upstreamBytes)
	writeScalar("downstream_bytes_total", "counter", m.downstreamBytes)
	writeScalar("active_requests", "gauge", m.activeRequests)
	writeScalar("active_waits", "gauge", m.activeWaits)
	writeScalar("active_connections", "gauge", p.connectionCount())
	writeScalar("cache_entries", "gauge", cs.Entries)
	writeScalar("cache_fills", "gauge", cs.Fills)
	writeScalar("cache_bytes", "gauge", cs.Bytes)
	writeScalar("cache_reserved_bytes", "gauge", cs.Reserved)
	writeScalar("cache_readers", "gauge", cs.Readers)
	writeScalar("cache_evictions_total", "counter", cs.Evictions)
	writeScalar("store_fill_success_total", "counter", cs.FillSuccess)
	writeScalar("store_fill_failure_total", "counter", cs.FillFailure)
	writeScalar("store_fill_abort_total", "counter", cs.FillAbort)
	writeScalar("store_fill_expiry_total", "counter", cs.FillExpiry)
	writeScalar("heap_alloc_bytes", "gauge", heapAlloc)
	writeScalar("goroutines", "gauge", runtime.NumGoroutine())
	b.WriteString("# TYPE docker_accelerator_cache_admissions_total counter\n")
	for reason := cache.AdmissionReason(0); reason < cache.AdmissionReasonCount; reason++ {
		fmt.Fprintf(&b, "docker_accelerator_cache_admissions_total{reason=%q} %d\n", reason.String(), cs.Admissions[reason])
	}
	b.WriteString("# TYPE docker_accelerator_request_duration_seconds histogram\n")
	var cumulative uint64
	for i, count := range m.durations {
		cumulative += count
		label := "+Inf"
		if i < len(requestDurationBounds) {
			label = strconv.FormatFloat(requestDurationBounds[i].Seconds(), 'g', -1, 64)
		}
		fmt.Fprintf(&b, "docker_accelerator_request_duration_seconds_bucket{le=%q} %d\n", label, cumulative)
	}
	fmt.Fprintf(&b, "docker_accelerator_request_duration_seconds_count %d\ndocker_accelerator_request_duration_seconds_sum %g\n", cumulative, m.durationSum)
	_, _ = fmt.Fprint(w, b.String())
}
func cloneMap(x map[string]uint64) map[string]uint64 {
	y := make(map[string]uint64, len(x))
	for k, v := range x {
		y[k] = v
	}
	return y
}
func cacheStats(p *Proxy) cache.Stats {
	if p.cache == nil {
		return cache.Stats{}
	}
	return p.cache.Stats()
}
func (p *Proxy) connectionCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.conns) }
func (m *metrics) finish(s *requestStats, outcome string, status int, d time.Duration, downstream int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRequests--
	m.requests[outcome]++
	m.statuses[statusClass(status)]++
	m.cache[s.cache]++
	if s.fill != "none" {
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
