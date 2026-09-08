package proxy

import (
	"fmt"
	"net/http"
	"runtime"
	runtimemetrics "runtime/metrics"
	"sort"
	"strings"
	"sync"
	"time"

	"ampcode.com/buildkite/docker-accelerator/internal/cache"
)

// heapObjectsMetric is the live heap-object byte count. Reading it via
// runtime/metrics avoids the stop-the-world pause that runtime.ReadMemStats
// imposes on every scrape.
const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

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
	durations                      [8]uint64
	durationSum                    float64
	activeRequests, activeWaits    int64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]uint64{}, statuses: map[string]uint64{}, cache: map[string]uint64{}, fills: map[string]uint64{}, waits: map[string]uint64{}, fallback: map[string]uint64{}, rejects: map[string]uint64{}}
}

type metricsSnapshot struct {
	requests, statuses, cache, fills, waits, fallback, rejects map[string]uint64
	upstream, upstreamBytes, downstreamBytes                   uint64
	durations                                                  [8]uint64
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
	for _, name := range []string{"upstream_get_attempts_total", "upstream_body_bytes_total", "downstream_bytes_total", "cache_evictions_total", "store_fill_success_total", "store_fill_failure_total", "store_fill_abort_total", "store_fill_expiry_total", "cache_admissions_total"} {
		fmt.Fprintf(&b, "# TYPE docker_accelerator_%s counter\n", name)
	}
	for _, name := range []string{"active_requests", "active_waits", "active_connections", "cache_entries", "cache_fills", "cache_bytes", "cache_reserved_bytes", "cache_readers", "heap_alloc_bytes", "goroutines"} {
		fmt.Fprintf(&b, "# TYPE docker_accelerator_%s gauge\n", name)
	}
	fmt.Fprintf(&b, "docker_accelerator_upstream_get_attempts_total %d\ndocker_accelerator_upstream_body_bytes_total %d\ndocker_accelerator_downstream_bytes_total %d\ndocker_accelerator_active_requests %d\ndocker_accelerator_active_waits %d\ndocker_accelerator_active_connections %d\n", m.upstream, m.upstreamBytes, m.downstreamBytes, m.activeRequests, m.activeWaits, p.connectionCount())
	fmt.Fprintf(&b, "docker_accelerator_cache_entries %d\ndocker_accelerator_cache_fills %d\ndocker_accelerator_cache_bytes %d\ndocker_accelerator_cache_reserved_bytes %d\ndocker_accelerator_cache_readers %d\ndocker_accelerator_cache_evictions_total %d\ndocker_accelerator_store_fill_success_total %d\ndocker_accelerator_store_fill_failure_total %d\ndocker_accelerator_store_fill_abort_total %d\ndocker_accelerator_store_fill_expiry_total %d\ndocker_accelerator_heap_alloc_bytes %d\ndocker_accelerator_goroutines %d\n", cs.Entries, cs.Fills, cs.Bytes, cs.Reserved, cs.Readers, cs.Evictions, cs.FillSuccess, cs.FillFailure, cs.FillAbort, cs.FillExpiry, heapAlloc, runtime.NumGoroutine())
	for reason := cache.AdmissionReason(0); reason < cache.AdmissionReasonCount; reason++ {
		fmt.Fprintf(&b, "docker_accelerator_cache_admissions_total{reason=%q} %d\n", reason.String(), cs.Admissions[reason])
	}
	bounds := []string{"0.01", "0.05", "0.1", "0.25", "0.5", "1", "5", "+Inf"}
	b.WriteString("# TYPE docker_accelerator_request_duration_seconds histogram\n")
	var cumulative uint64
	for i, count := range m.durations {
		cumulative += count
		fmt.Fprintf(&b, "docker_accelerator_request_duration_seconds_bucket{le=%q} %d\n", bounds[i], cumulative)
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
	bounds := []time.Duration{.01e9, .05e9, .1e9, .25e9, .5e9, 1e9, 5e9}
	i := 0
	for i < len(bounds) && d > bounds[i] {
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
