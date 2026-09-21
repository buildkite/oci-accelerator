package integration

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type processLoad struct{ rss, fds, heap, goroutines int64 }
type loadPeaks struct {
	rss, fds, heap, goroutines                     int64
	cacheCharged, cacheReserved, cacheReservations int64
	fills, entries, entrySlots, connections        int64
}

func readProcessLoad(t *testing.T, pidFile string) processLoad {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	status, err := os.ReadFile("/proc/" + pid + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var p processLoad
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			p.rss, _ = strconv.ParseInt(f[1], 10, 64)
			p.rss *= 1024
		}
	}
	ents, err := os.ReadDir("/proc/" + pid + "/fd")
	if err != nil {
		t.Fatal(err)
	}
	p.fds = int64(len(ents))
	return p
}

func sampleLoad(ctx context.Context, port int, pidFile string, maxBytes int64, maxEntries, maxFills, maxConnections int, peaks *loadPeaks, ready chan<- struct{}, done chan<- struct{}, errs chan<- error) {
	defer close(done)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	first := true
	for {
		m, err := fetchLoadMetrics(port)
		if err == nil {
			err = recordLoadSample(pidFile, m, maxBytes, maxEntries, maxFills, maxConnections, peaks)
		}
		if err != nil {
			errs <- err
			return
		}
		if first {
			close(ready)
			first = false
		}
		select {
		case <-ctx.Done():
			// Record after cancellation too: the preceding tick may predate the
			// last completed request or final publication.
			m, err := fetchLoadMetrics(port)
			if err == nil {
				err = recordLoadSample(pidFile, m, maxBytes, maxEntries, maxFills, maxConnections, peaks)
			}
			if err != nil {
				errs <- err
			}
			return
		case <-ticker.C:
		}
	}
}

func recordLoadSample(pidFile string, m map[string]float64, maxBytes int64, maxEntries, maxFills, maxConnections int, peaks *loadPeaks) error {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("read sampler pid: %w", err)
	}
	pid := strings.TrimSpace(string(b))
	status, err := os.ReadFile("/proc/" + pid + "/status")
	if err != nil {
		return fmt.Errorf("read sampler status: %w", err)
	}
	var rss int64
	foundRSS := false
	for _, l := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			f := strings.Fields(l)
			if len(f) < 2 {
				return fmt.Errorf("malformed VmRSS status line %q", l)
			}
			rss, err = strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return fmt.Errorf("parse VmRSS: %w", err)
			}
			rss *= 1024
			foundRSS = true
		}
	}
	if !foundRSS {
		return fmt.Errorf("process status missing VmRSS")
	}
	fds, err := os.ReadDir("/proc/" + pid + "/fd")
	if err != nil {
		return fmt.Errorf("read sampler fds: %w", err)
	}
	charged := int64(m["oci_accelerator_cache_bytes"] + m["oci_accelerator_cache_reserved_bytes"])
	reservations := int64(m["oci_accelerator_cache_fills"])
	entrySlots := int64(m["oci_accelerator_cache_entries"] + m["oci_accelerator_cache_fills"])
	peaks.rss = max64(peaks.rss, rss)
	peaks.fds = max64(peaks.fds, int64(len(fds)))
	peaks.heap = max64(peaks.heap, int64(m["oci_accelerator_heap_alloc_bytes"]))
	peaks.goroutines = max64(peaks.goroutines, int64(m["oci_accelerator_goroutines"]))
	peaks.cacheCharged = max64(peaks.cacheCharged, charged)
	peaks.cacheReserved = max64(peaks.cacheReserved, int64(m["oci_accelerator_cache_reserved_bytes"]))
	peaks.cacheReservations = max64(peaks.cacheReservations, reservations)
	peaks.fills = max64(peaks.fills, int64(m["oci_accelerator_cache_fills"]))
	peaks.entries = max64(peaks.entries, int64(m["oci_accelerator_cache_entries"]))
	peaks.entrySlots = max64(peaks.entrySlots, entrySlots)
	peaks.connections = max64(peaks.connections, int64(m["oci_accelerator_active_connections"]))
	if charged > maxBytes || entrySlots > int64(maxEntries) || reservations > int64(maxFills) || peaks.connections > int64(maxConnections) {
		return fmt.Errorf("bounded-resource gauge exceeded configured limit: %v", m)
	}
	return nil
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func fetchLoadMetrics(port int) (map[string]float64, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics status %s", r.Status)
	}
	m := map[string]float64{}
	s := bufio.NewScanner(r.Body)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "oci_accelerator_") {
			f := strings.Fields(line)
			if len(f) == 2 {
				v, err := strconv.ParseFloat(f[1], 64)
				if err != nil {
					return nil, err
				}
				m[f[0]] = v
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	for _, key := range []string{"oci_accelerator_active_requests", "oci_accelerator_active_waits", "oci_accelerator_active_connections", "oci_accelerator_cache_fills", "oci_accelerator_cache_readers", "oci_accelerator_cache_reserved_bytes", "oci_accelerator_cache_bytes", "oci_accelerator_cache_entries", "oci_accelerator_heap_alloc_bytes", "oci_accelerator_goroutines"} {
		if _, ok := m[key]; !ok {
			return nil, fmt.Errorf("metrics response missing %s", key)
		}
	}
	return m, nil
}
func metricDelta(before, after map[string]float64, key string) float64 {
	return after[key] - before[key]
}

func waitLoadQuiescent(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	names := []string{"oci_accelerator_active_requests", "oci_accelerator_active_waits", "oci_accelerator_active_connections", "oci_accelerator_cache_fills", "oci_accelerator_cache_readers", "oci_accelerator_cache_reserved_bytes"}
	for time.Now().Before(deadline) {
		m, e := fetchLoadMetrics(port)
		ok := e == nil
		for _, n := range names {
			ok = ok && m[n] == 0
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("proxy did not quiesce")
}

func diskUsage(t *testing.T, root string) (int64, int64) {
	t.Helper()
	var logical, physical int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			logical += info.Size()
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			physical += st.Blocks * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return logical, physical
}
