package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const loadAuthPrefix = "Bearer load-client-"

type loadResult struct {
	Case                    string  `json:"case"`
	Phase                   string  `json:"phase"`
	Profile                 string  `json:"profile"`
	Clients                 int     `json:"clients"`
	ObjectBytes             int64   `json:"object_bytes"`
	Distinct                bool    `json:"distinct"`
	P50MS                   float64 `json:"p50_ms"`
	P95MS                   float64 `json:"p95_ms"`
	WarmupGETs              int64   `json:"warmup_gets"`
	WarmupBodyBytes         int64   `json:"warmup_body_bytes"`
	OriginGETs              int64   `json:"origin_gets"`
	OriginBodyBytes         int64   `json:"origin_body_bytes"`
	WaitHitDelta            float64 `json:"wait_hit_delta"`
	FallbackDelta           float64 `json:"fallback_delta"`
	LogicalDiskBytes        int64   `json:"logical_disk_bytes"`
	PhysicalDiskBytes       int64   `json:"physical_disk_bytes"`
	PeakRSSBytes            int64   `json:"peak_rss_bytes"`
	PeakFDs                 int64   `json:"peak_fds"`
	PeakHeapBytes           int64   `json:"peak_heap_bytes"`
	PeakGoroutines          int64   `json:"peak_goroutines"`
	PeakCacheChargedBytes   int64   `json:"peak_cache_charged_bytes"`
	PeakCacheReservedBytes  int64   `json:"peak_cache_reserved_bytes"`
	PeakCacheReservations   int64   `json:"peak_cache_reservations"`
	PeakCacheFills          int64   `json:"peak_cache_fills"`
	PeakCacheEntries        int64   `json:"peak_cache_entries"`
	PeakCacheEntrySlots     int64   `json:"peak_cache_entry_slots"`
	PeakActiveConnections   int64   `json:"peak_active_connections"`
	BaselineFDs             int64   `json:"baseline_fds"`
	EndFDs                  int64   `json:"end_fds"`
	BaselineGoroutines      int64   `json:"baseline_goroutines"`
	EndGoroutines           int64   `json:"end_goroutines"`
	FinalCacheFills         int64   `json:"final_cache_fills"`
	FinalCacheReaders       int64   `json:"final_cache_readers"`
	FinalCacheReservedBytes int64   `json:"final_cache_reserved_bytes"`
	FinalActiveRequests     int64   `json:"final_active_requests"`
	FinalActiveWaits        int64   `json:"final_active_waits"`
	FinalActiveConnections  int64   `json:"final_active_connections"`
}

type loadCounters struct {
	gets, body, badAuth, paceNanos, active atomic.Int64
	mu                                     sync.Mutex
	authed                                 map[string]int64
}

// TestBoundedHTTPLoadAcceptance is deliberately opt-in: its complete source
// matrix transfers tens of GiB and exercises the production binary and cache.
func TestBoundedHTTPLoadAcceptance(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_LOAD_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_LOAD_E2E=1 to run the bounded HTTP load matrix")
	}
	requireLinux(t)
	requireCommands(t, "go")
	root := t.TempDir()
	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	ip := nonLoopbackIPv4(t)
	counters := &loadCounters{authed: make(map[string]int64)}
	origin, originPort, originCA := startNamedTLS(t, root, "load-origin-ca", ip.String(), ip, loadOrigin(counters))
	defer origin.Close()
	authority := net.JoinHostPort(ip.String(), strconv.Itoa(originPort))

	var results []loadResult
	for _, clients := range []int{1, 8, 32} {
		for _, size := range []int64{8 << 20, 64 << 20} {
			for _, distinct := range []bool{false, true} {
				for _, phase := range []string{"cold", "warm", "proxy-restart"} {
					label := fmt.Sprintf("c%d-%dMiB-%s-%s", clients, size>>20, map[bool]string{false: "same", true: "distinct"}[distinct], phase)
					t.Run(label, func(t *testing.T) {
						r := runLoadCase(t, bin, originCA, authority, counters, clients, size, distinct, phase, "normal", time.Millisecond, 5*time.Second, 16, 96)
						results = append(results, r)
					})
				}
			}
		}
	}
	// Explicitly exercise both coalescing outcomes rather than relying on timing
	// in the broad matrix to happen to cover them.
	for _, p := range []struct {
		name       string
		pace, wait time.Duration
	}{{"wait-hit", 5 * time.Millisecond, 5 * time.Second}, {"timeout-fallback", 10 * time.Millisecond, time.Millisecond}} {
		t.Run(p.name, func(t *testing.T) {
			results = append(results, runLoadCase(t, bin, originCA, authority, counters, 32, 8<<20, false, "cold", p.name, p.pace, p.wait, 16, 96))
		})
	}
	t.Run("tiny-entry-cap-churn", func(t *testing.T) {
		results = append(results, runLoadCase(t, bin, originCA, authority, counters, 32, 4<<10, true, "cold", "entry-cap-churn", 0, 5*time.Second, 16, 8))
	})
	writeLoadReport(t, results)
}

func loadOrigin(c *loadCounters) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.active.Add(1)
		defer c.active.Add(-1)
		c.gets.Add(1)
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Header.Get("Authorization"), loadAuthPrefix) {
			c.badAuth.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		c.authed[r.Header.Get("Authorization")]++
		c.mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/load/"), "/")
		if len(parts) != 4 || parts[2] != "blobs" {
			http.NotFound(w, r)
			return
		}
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || n < 0 || n > 64<<20 {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
		seed := parts[1]
		if parts[3] != "sha256:"+loadDigest(n, seed) {
			http.Error(w, "bad digest", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", "sha256:"+loadDigest(n, seed))
		reader := io.Reader(&loadReader{remaining: n, seed: loadSeed(seed)})
		if delay := time.Duration(c.paceNanos.Load()); delay > 0 {
			reader = &pacedLoadReader{r: reader, delay: delay}
		}
		written, _ := io.Copy(w, reader)
		c.body.Add(written)
	})
}

type loadReader struct {
	remaining, offset int64
	seed              byte
}

func (r *loadReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = byte((r.offset+int64(i))*131 + int64(r.seed)*17)
	}
	r.offset += int64(len(p))
	r.remaining -= int64(len(p))
	return len(p), nil
}

type pacedLoadReader struct {
	r     io.Reader
	delay time.Duration
}

func (r *pacedLoadReader) Read(p []byte) (int, error) {
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := r.r.Read(p)
	if n > 0 {
		time.Sleep(r.delay)
	}
	return n, err
}
func loadSeed(s string) byte { h := sha256.Sum256([]byte(s)); return h[0] }
func loadDigest(n int64, seed string) string {
	h := sha256.New()
	_, _ = io.Copy(h, &loadReader{remaining: n, seed: loadSeed(seed)})
	return hex.EncodeToString(h.Sum(nil))
}

func runLoadCase(t *testing.T, bin, originCA, authority string, counters *loadCounters, clients int, size int64, distinct bool, phase, profile string, pace, wait time.Duration, maxFills, maxEntries int) loadResult {
	t.Helper()
	caseRoot := t.TempDir()
	proxyPort, metricsPort := freePort(t, "127.0.0.1"), freePort(t, "127.0.0.1")
	cacheDir, proxyCA := filepath.Join(caseRoot, "cache"), filepath.Join(caseRoot, "ca")
	pidFile, config := filepath.Join(caseRoot, "proxy.pid"), filepath.Join(caseRoot, "proxy.yaml")
	maxBytes := int64(2<<30) + 1<<20
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nmetrics_listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: %d\n  max_fills: %d\n  max_entries: %d\n  fill_wait_timeout: %s\nmax_connections: 64\n", proxyPort, metricsPort, authority, proxyCA, cacheDir, maxBytes, maxFills, maxEntries, wait), 0o600)
	command := fmt.Sprintf("env SSL_CERT_FILE=%s %s --config %s", shell(originCA), shell(bin), shell(config))
	running := false
	var process *testProcess
	start := func() {
		process = startProcess(t, command)
		process.writePIDFile(t, pidFile)
		running = true
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", metricsPort))
	}
	stop := func() {
		if running {
			if process.Stop(t) {
				running = false
			}
		}
	}
	t.Cleanup(stop)
	start()
	client := faultProxyClient(t, proxyPort, filepath.Join(proxyCA, "ca.crt"))
	client.Timeout = 60 * time.Second
	transport := client.Transport.(*http.Transport)
	var warmupGETs, warmupBody int64

	// Warm/restart population is explicit and reported separately; the measured
	// phase never hides these extra authenticated origin GETs.
	if phase != "cold" {
		b, bb := counters.gets.Load(), counters.body.Load()
		counters.paceNanos.Store(0)
		keys := 1
		if distinct {
			keys = clients
		}
		for i := 0; i < keys; i++ {
			doLoadBatch(t, client, authority, 1, size, distinct, i)
		}
		waitOriginIdle(t, counters)
		warmupGETs, warmupBody = counters.gets.Load()-b, counters.body.Load()-bb
		transport.CloseIdleConnections()
		waitLoadQuiescent(t, metricsPort)
		if phase == "proxy-restart" {
			stop()
			start()
			client = faultProxyClient(t, proxyPort, filepath.Join(proxyCA, "ca.crt"))
			client.Timeout = 60 * time.Second
			transport = client.Transport.(*http.Transport)
		}
	}
	beforeGET, beforeBody := counters.gets.Load(), counters.body.Load()
	beforeMetrics, err := fetchLoadMetrics(metricsPort)
	if err != nil {
		t.Fatal(err)
	}
	base := readProcessLoad(t, pidFile)
	base.goroutines = int64(beforeMetrics["oci_accelerator_goroutines"])
	counters.mu.Lock()
	beforeAuth := make(map[string]int64, len(counters.authed))
	for k, v := range counters.authed {
		beforeAuth[k] = v
	}
	counters.mu.Unlock()
	counters.paceNanos.Store(int64(pace))
	defer counters.paceNanos.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peaks := loadPeaks{}
	done := make(chan struct{})
	sampleErr := make(chan error, 1)
	ready := make(chan struct{})
	go sampleLoad(ctx, metricsPort, pidFile, maxBytes, maxEntries, maxFills, 64, &peaks, ready, done, sampleErr)
	select {
	case <-ready:
	case err := <-sampleErr:
		t.Fatalf("load sampler startup: %v", err)
	}
	latencies := doLoadBatch(t, client, authority, clients, size, distinct, 0)
	if profile == "entry-cap-churn" {
		doLoadBatch(t, client, authority, 9, size, true, clients)
	}
	transport.CloseIdleConnections()
	waitLoadQuiescent(t, metricsPort)
	waitOriginIdle(t, counters)
	cancel()
	<-done
	select {
	case err := <-sampleErr:
		t.Error(err)
	default:
	}
	if counters.badAuth.Load() != 0 {
		t.Fatalf("origin observed %d unauthorized requests", counters.badAuth.Load())
	}
	end := readProcessLoad(t, pidFile)
	afterMetrics, err := fetchLoadMetrics(metricsPort)
	if err != nil {
		t.Fatal(err)
	}
	end.goroutines = int64(afterMetrics["oci_accelerator_goroutines"])
	gets, body := counters.gets.Load()-beforeGET, counters.body.Load()-beforeBody
	fallbacks := metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_fallbacks_total{outcome="retry"}`)
	expectedClients := clients
	if profile == "entry-cap-churn" {
		expectedClients += 9
	}
	if gets != int64(expectedClients)+int64(fallbacks) {
		t.Errorf("origin GETs=%d, want clients(%d)+fallbacks(%g)", gets, expectedClients, fallbacks)
	}
	counters.mu.Lock()
	for i := 0; i < clients; i++ {
		key := loadAuthPrefix + strconv.Itoa(i)
		if counters.authed[key]-beforeAuth[key] < 1 {
			t.Errorf("client %d had no authenticated upstream GET", i)
		}
	}
	counters.mu.Unlock()
	waits := metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_cache_total{outcome="wait_hit"}`)
	if profile == "wait-hit" && (waits <= 0 || fallbacks != 0) {
		t.Errorf("wait-hit profile: waits=%g fallbacks=%g", waits, fallbacks)
	}
	if profile == "timeout-fallback" && fallbacks <= 0 {
		t.Errorf("timeout-fallback profile had no fallback")
	}
	if phase != "cold" && body >= int64(clients)*size/4 {
		t.Errorf("warm origin body=%d, want <%d", body, int64(clients)*size/4)
	}
	if phase == "cold" && gets > int64(2*clients) {
		t.Errorf("cold origin GETs=%d exceed %d", gets, 2*clients)
	}
	if profile == "entry-cap-churn" && (afterMetrics["oci_accelerator_cache_entries"] > 8 || metricDelta(beforeMetrics, afterMetrics, "oci_accelerator_cache_evictions_total") <= 0) {
		t.Errorf("entry-cap churn missing: entries=%g evictions=%g", afterMetrics["oci_accelerator_cache_entries"], metricDelta(beforeMetrics, afterMetrics, "oci_accelerator_cache_evictions_total"))
	}
	if end.fds > base.fds+16 {
		t.Errorf("FDs did not return near baseline: baseline=%d end=%d", base.fds, end.fds)
	}
	if end.goroutines > base.goroutines+32 {
		t.Errorf("goroutines did not return near baseline: baseline=%d end=%d", base.goroutines, end.goroutines)
	}
	logical, physical := diskUsage(t, cacheDir)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	r := loadResult{Case: t.Name(), Phase: phase, Profile: profile, Clients: clients, ObjectBytes: size, Distinct: distinct, P50MS: percentile(latencies, .50), P95MS: percentile(latencies, .95), WarmupGETs: warmupGETs, WarmupBodyBytes: warmupBody, OriginGETs: gets, OriginBodyBytes: body, WaitHitDelta: waits, FallbackDelta: fallbacks, LogicalDiskBytes: logical, PhysicalDiskBytes: physical, PeakRSSBytes: peaks.rss, PeakFDs: peaks.fds, PeakHeapBytes: peaks.heap, PeakGoroutines: peaks.goroutines, PeakCacheChargedBytes: peaks.cacheCharged, PeakCacheReservedBytes: peaks.cacheReserved, PeakCacheReservations: peaks.cacheReservations, PeakCacheFills: peaks.fills, PeakCacheEntries: peaks.entries, PeakCacheEntrySlots: peaks.entrySlots, PeakActiveConnections: peaks.connections, BaselineFDs: base.fds, EndFDs: end.fds, BaselineGoroutines: base.goroutines, EndGoroutines: end.goroutines, FinalCacheFills: int64(afterMetrics["oci_accelerator_cache_fills"]), FinalCacheReaders: int64(afterMetrics["oci_accelerator_cache_readers"]), FinalCacheReservedBytes: int64(afterMetrics["oci_accelerator_cache_reserved_bytes"]), FinalActiveRequests: int64(afterMetrics["oci_accelerator_active_requests"]), FinalActiveWaits: int64(afterMetrics["oci_accelerator_active_waits"]), FinalActiveConnections: int64(afterMetrics["oci_accelerator_active_connections"])}
	t.Logf("LOAD case=%s profile=%s clients=%d bytes=%d distinct=%t p50_ms=%.2f p95_ms=%.2f origin_gets=%d origin_body_bytes=%d disk_logical=%d disk_physical=%d peak_rss=%d peak_fds=%d peak_heap=%d peak_goroutines=%d", r.Case, profile, clients, size, distinct, r.P50MS, r.P95MS, r.OriginGETs, r.OriginBodyBytes, logical, physical, r.PeakRSSBytes, r.PeakFDs, r.PeakHeapBytes, r.PeakGoroutines)
	t.Logf("AUDIT baseline_fd=%d end_fd=%d baseline_goroutines=%d end_goroutines=%d cache_entries=%g cache_bytes=%g cache_fills=%g active_connections=%g", base.fds, end.fds, base.goroutines, end.goroutines, afterMetrics["oci_accelerator_cache_entries"], afterMetrics["oci_accelerator_cache_bytes"], afterMetrics["oci_accelerator_cache_fills"], afterMetrics["oci_accelerator_active_connections"])
	return r
}

func doLoadBatch(t *testing.T, client *http.Client, authority string, clients int, size int64, distinct bool, seedOffset int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	ready := make(chan struct{})
	digests := make([]string, clients)
	for i := range clients {
		seed := "same"
		if distinct {
			seed = fmt.Sprintf("key-%d", i+seedOffset)
		}
		digests[i] = loadDigest(size, seed)
	}
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			seed := "same"
			if distinct {
				seed = fmt.Sprintf("key-%d", i+seedOffset)
			}
			digest := digests[i]
			target := fmt.Sprintf("https://%s/v2/load/%d/%s/blobs/sha256:%s", authority, size, seed, digest)
			req, _ := http.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Authorization", loadAuthPrefix+strconv.Itoa(i))
			started := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				errs <- fmt.Errorf("client %d: %w", i, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("client %d status %s", i, resp.Status)
				return
			}
			h := sha256.New()
			n, err := io.Copy(h, resp.Body)
			out[i] = time.Since(started)
			if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != digest {
				errs <- fmt.Errorf("client %d exact response failed: bytes=%d/%d digest=%x want=%s err=%v", i, n, size, h.Sum(nil), digest, err)
			}
		}(i)
	}
	close(ready)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	return out
}

func waitOriginIdle(t *testing.T, c *loadCounters) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c.active.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("origin requests did not finish")
}
func percentile(v []time.Duration, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	i := int(float64(len(v)-1)*p + .5)
	return float64(v[i]) / float64(time.Millisecond)
}

func writeLoadReport(t *testing.T, results []loadResult) {
	t.Helper()
	path := os.Getenv("OCI_ACCELERATOR_LOAD_REPORT")
	if path == "" || t.Failed() || len(results) != 39 {
		return
	}
	writeLoadJSON(t, path, results)
	t.Logf("load report: %s", path)
}

// Only complete passing matrices call this. Publish atomically so a failed
// write cannot leave a report that appears to contain a successful run.
func writeLoadJSON(t *testing.T, path string, results any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".load-report-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("OCI_ACCELERATOR_LOAD_REPORT", path)
	t.Setenv("OCI_ACCELERATOR_DOCKER_LOAD_REPORT", path)
	writeLoadReport(t, make([]loadResult, 38))
	writeDockerLoadReport(t, make([]dockerLoadResult, 5))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("incomplete selection published a report")
	}
	writeLoadReport(t, make([]loadResult, 39))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []loadResult
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 39 {
		t.Fatalf("invalid complete report: %v rows=%d", err, len(rows))
	}
	if files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".load-report-*")); err != nil || len(files) != 0 {
		t.Fatalf("temporary reports leaked: %v %v", files, err)
	}
}
