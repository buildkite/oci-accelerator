package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDockerLoadAcceptance exercises contention through real, independent VFS
// daemons. It is deliberately separate from the ordinary Docker acceptance test
// because it starts fifteen dockerd instances over its full run.
func TestDockerLoadAcceptance(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_LOAD_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_LOAD_E2E=1 to run the Docker load acceptance test")
	}
	requireLinux(t)
	requireCommands(t, "docker", "dockerd", "sudo", "mount")
	if os.Geteuid() != 0 {
		run(t, "sudo", "-n", "true")
	}

	root := t.TempDir()
	image := makeImageWithPayload(t, deterministicLargePayload())
	origin, image, counts := startRegistry(t, root, nonLoopbackIPv4(t), image)
	defer origin.Close()
	authority := origin.Listener.Addr().String()

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	metricsPort := freePort(t, "127.0.0.1")
	proxyCA := filepath.Join(root, "proxy-ca")
	cacheDir := filepath.Join(root, "cache")
	pidFile := filepath.Join(root, "proxy.pid")
	config := filepath.Join(root, "proxy.env")
	writeFile(t, config, fmt.Sprintf("OCI_ACCELERATOR_LISTEN=127.0.0.1:%d\nOCI_ACCELERATOR_METRICS_LISTEN=127.0.0.1:%d\nOCI_ACCELERATOR_REGISTRIES=%s\nOCI_ACCELERATOR_CA_DIR=%s\nOCI_ACCELERATOR_CACHE_DIR=%s\nOCI_ACCELERATOR_CACHE_MAX_BYTES=33554432\nOCI_ACCELERATOR_CACHE_MAX_ENTRIES=100000\nOCI_ACCELERATOR_CACHE_MAX_FILLS=16\nOCI_ACCELERATOR_CACHE_AUTHORIZATION_TTL=0s\nOCI_ACCELERATOR_CACHE_ANONYMOUS_AUTHORIZATION_TTL=0s\nOCI_ACCELERATOR_MAX_CONNECTIONS=256\n", proxyPort, metricsPort, authority, proxyCA, cacheDir), 0o600)

	prefix := fmt.Sprintf("acc4e-%06x", uint64(time.Now().UnixNano())&0xffffff)
	proxyCommand := fmt.Sprintf("env SSL_CERT_FILE=%s %s --env-file %s", shell(originCAPath(root)), shell(bin), shell(config))
	proxy := startProcess(t, proxyCommand)
	proxy.writePIDFile(t, pidFile)
	proxyRunning := true
	t.Cleanup(func() {
		if proxyRunning {
			proxy.Stop(t)
		}
	})
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", metricsPort))
	caFingerprint := certFingerprint(t, filepath.Join(proxyCA, "ca.crt"))

	var coldLayerBytes int64
	var results []dockerLoadResult
	serial := 0
	for _, clients := range []int{1, 4} {
		if clients == 4 {
			if !proxy.Stop(t) {
				t.Fatal("cannot reset cache after failed proxy stop")
			}
			proxyRunning = false
			if err := os.RemoveAll(cacheDir); err != nil {
				t.Fatalf("reset cache for four-client cold phase: %v", err)
			}
			proxy = startProcess(t, proxyCommand)
			proxy.writePIDFile(t, pidFile)
			proxyRunning = true
			waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
			if got := certFingerprint(t, filepath.Join(proxyCA, "ca.crt")); got != caFingerprint {
				t.Fatalf("proxy CA changed across workload reset: %s != %s", got, caFingerprint)
			}
		}
		for phaseIndex, phase := range []string{"cold", "warm", "restart"} {
			if phase == "restart" {
				if !proxy.Stop(t) {
					t.Fatal("cannot restart proxy after failed stop")
				}
				proxyRunning = false
				proxy = startProcess(t, proxyCommand)
				proxy.writePIDFile(t, pidFile)
				proxyRunning = true
				waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
				if got := certFingerprint(t, filepath.Join(proxyCA, "ca.crt")); got != caFingerprint {
					t.Fatalf("proxy CA changed across restart: %s != %s", got, caFingerprint)
				}
			}

			beforeA, beforeB, _, _ := counts.snapshot()
			beforeTags := counts.tagCount("/v2/tiny/manifests/latest")
			phaseStart := time.Now()
			type pull struct {
				socket, client string
				stop           func()
			}
			pulls := make([]pull, clients)
			stopped := false
			defer func() {
				if !stopped {
					for i := range pulls {
						if pulls[i].stop != nil {
							pulls[i].stop()
						}
					}
				}
			}()
			for i := range pulls {
				serial++
				clientDir := filepath.Join(root, fmt.Sprintf("client-%02d", serial))
				if err := os.Mkdir(clientDir, 0o700); err != nil {
					t.Fatal(err)
				}
				auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPass))
				writeFile(t, filepath.Join(clientDir, "config.json"), fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, authority, auth), 0o600)
				socket, stop := startDaemon(t, fmt.Sprintf("%s-%d-%s-%d", prefix, clients, phase, i), proxyPort, filepath.Join(proxyCA, "ca.crt"))
				pulls[i] = pull{socket: socket, client: clientDir, stop: stop}
			}
			beforeMetrics, err := fetchLoadMetrics(metricsPort)
			if err != nil {
				t.Fatal(err)
			}
			base := readProcessLoad(t, pidFile)
			base.goroutines = int64(beforeMetrics["oci_accelerator_goroutines"])
			ctx, cancel := context.WithCancel(context.Background())
			peaks := loadPeaks{}
			done, sampleErr, ready := make(chan struct{}), make(chan error, 1), make(chan struct{})
			go sampleLoad(ctx, metricsPort, pidFile, 32<<20, 100000, 16, 256, &peaks, ready, done, sampleErr)
			samplerJoined := false
			defer func() {
				cancel()
				if !samplerJoined {
					<-done
				}
			}()
			select {
			case <-ready:
			case err := <-sampleErr:
				t.Fatalf("load sampler startup: %v", err)
			}

			// Closing start is the genuine pull barrier: every daemon has answered
			// docker info and all fresh client configuration exists before this.
			start := make(chan struct{})
			latencies := make([]time.Duration, clients)
			errs := make([]error, clients)
			var wg sync.WaitGroup
			for i := range pulls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					began := time.Now()
					errs[i] = load4ePullAndInspect(pulls[i].socket, pulls[i].client, authority, image)
					latencies[i] = time.Since(began)
				}(i)
			}
			close(start)
			wg.Wait()
			for i := range pulls {
				pulls[i].stop()
				if errs[i] != nil {
					t.Errorf("%d-client %s pull %d: %v", clients, phase, i, errs[i])
				}
			}
			stopped = true
			counts.waitIdle(t)
			waitLoadQuiescent(t, metricsPort)
			cancel()
			<-done
			samplerJoined = true
			select {
			case err := <-sampleErr:
				t.Fatal(err)
			default:
			}
			if t.Failed() {
				t.FailNow()
			}
			afterMetrics, err := fetchLoadMetrics(metricsPort)
			if err != nil {
				t.Fatal(err)
			}
			end := readProcessLoad(t, pidFile)
			end.goroutines = int64(afterMetrics["oci_accelerator_goroutines"])
			if end.fds > base.fds+16 {
				t.Errorf("FDs did not return near baseline: baseline=%d end=%d", base.fds, end.fds)
			}
			if end.goroutines > base.goroutines+32 {
				t.Errorf("goroutines did not return near baseline: baseline=%d end=%d", base.goroutines, end.goroutines)
			}
			afterA, afterB, badAuth, _ := counts.snapshot()
			if badAuth != 0 {
				t.Fatalf("origin received %d malformed Authorization headers", badAuth)
			}
			for _, path := range []string{image.layerPath, image.configPath} {
				if got := afterA[path] - beforeA[path]; got != clients {
					t.Fatalf("%d-client %s %s fresh upstream GETs=%d, want %d", clients, phase, path, got, clients)
				}
			}
			if got := counts.tagCount("/v2/tiny/manifests/latest") - beforeTags; got < clients {
				t.Fatalf("%d-client %s mutable-tag requests=%d, want at least %d", clients, phase, got, clients)
			}
			layerBytes := afterB[image.layerPath] - beforeB[image.layerPath]
			configBytes := afterB[image.configPath] - beforeB[image.configPath]
			if phaseIndex == 0 {
				if layerBytes < int64(len(image.layer)) {
					t.Fatalf("%d-client cold layer body bytes=%d, want at least one complete body (%d)", clients, layerBytes, len(image.layer))
				}
				coldLayerBytes = layerBytes
			} else if layerBytes >= coldLayerBytes {
				t.Fatalf("%d-client %s did not save origin layer body bytes: got=%d cold=%d", clients, phase, layerBytes, coldLayerBytes)
			}
			if phaseIndex > 0 && int64(clients)*int64(len(image.layer))-layerBytes < int64(clients)*int64(len(image.layer))*3/4 {
				t.Fatalf("%d-client %s layer byte savings=%d, want at least %d", clients, phase, int64(clients)*int64(len(image.layer))-layerBytes, int64(clients)*int64(len(image.layer))*3/4)
			}
			logical, physical := diskUsage(t, cacheDir)
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			r := dockerLoadResult{loadResult: loadResult{
				Case: t.Name(), Phase: phase, Profile: "docker", Clients: clients, ObjectBytes: int64(len(image.layer)),
				P50MS: percentile(latencies, .5), P95MS: percentile(latencies, .95),
				OriginGETs: int64(afterA[image.layerPath] - beforeA[image.layerPath] + afterA[image.configPath] - beforeA[image.configPath]), OriginBodyBytes: layerBytes + configBytes,
				WaitHitDelta:     metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_cache_total{outcome="wait_hit"}`),
				FallbackDelta:    metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_fallbacks_total{outcome="retry"}`),
				LogicalDiskBytes: logical, PhysicalDiskBytes: physical,
				PeakRSSBytes: peaks.rss, PeakFDs: peaks.fds, PeakHeapBytes: peaks.heap, PeakGoroutines: peaks.goroutines,
				PeakCacheChargedBytes: peaks.cacheCharged, PeakCacheReservedBytes: peaks.cacheReserved, PeakCacheReservations: peaks.cacheReservations,
				PeakCacheFills: peaks.fills, PeakCacheEntries: peaks.entries, PeakCacheEntrySlots: peaks.entrySlots, PeakActiveConnections: peaks.connections,
				BaselineFDs: base.fds, EndFDs: end.fds, BaselineGoroutines: base.goroutines, EndGoroutines: end.goroutines,
				FinalCacheFills: int64(afterMetrics["oci_accelerator_cache_fills"]), FinalCacheReaders: int64(afterMetrics["oci_accelerator_cache_readers"]),
				FinalCacheReservedBytes: int64(afterMetrics["oci_accelerator_cache_reserved_bytes"]), FinalActiveRequests: int64(afterMetrics["oci_accelerator_active_requests"]),
				FinalActiveWaits: int64(afterMetrics["oci_accelerator_active_waits"]), FinalActiveConnections: int64(afterMetrics["oci_accelerator_active_connections"]),
			}, LayerGETs: afterA[image.layerPath] - beforeA[image.layerPath], ConfigGETs: afterA[image.configPath] - beforeA[image.configPath],
				CacheHits:     metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_cache_total{outcome="hit"}`),
				CacheWaitHits: metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_cache_total{outcome="wait_hit"}`),
				CacheMisses:   metricDelta(beforeMetrics, afterMetrics, `oci_accelerator_cache_total{outcome="miss"}`)}
			results = append(results, r)
			load4eLogPhase(t, r, time.Since(phaseStart), latencies, layerBytes, configBytes, afterMetrics)
		}
	}

	finalMetrics, err := fetchLoadMetrics(metricsPort)
	if err != nil {
		t.Fatal(err)
	}
	for _, gauge := range []string{"oci_accelerator_cache_fills", "oci_accelerator_cache_reserved_bytes", "oci_accelerator_active_requests", "oci_accelerator_active_waits", "oci_accelerator_active_connections"} {
		if finalMetrics[gauge] != 0 {
			t.Errorf("final %s=%g, want 0", gauge, finalMetrics[gauge])
		}
	}
	if fills, err := filepath.Glob(filepath.Join(cacheDir, ".fill-*")); err != nil || len(fills) != 0 {
		t.Errorf("final temporary fills=%v (glob error %v)", fills, err)
	}
	if !proxy.Stop(t) {
		t.Fatal("failed to stop owned proxy service")
	}
	proxyRunning = false
	writeDockerLoadReport(t, results)
}

func load4ePullAndInspect(socket, clientDir, authority string, image fixtureImage) error {
	env := []string{"DOCKER_CONFIG=" + clientDir}
	if r := dockerCombined(socket, env, "pull", authority+"/tiny:latest"); r.err != nil {
		return fmt.Errorf("pull: %w: %s", r.err, trim(r.output))
	}
	r := dockerCombined(socket, env, "image", "inspect", authority+"/tiny:latest", "--format", "{{json .}}")
	if r.err != nil {
		return fmt.Errorf("inspect: %w: %s", r.err, trim(r.output))
	}
	var got struct {
		ID          string `json:"Id"`
		RootFS      struct{ Layers []string }
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.output)), &got); err != nil {
		return fmt.Errorf("decode inspect: %w", err)
	}
	if got.ID != image.configDigest || len(got.RootFS.Layers) != 1 || got.RootFS.Layers[0] != image.diffID {
		return fmt.Errorf("config/rootfs identity mismatch: id=%s layers=%v", got.ID, got.RootFS.Layers)
	}
	want := authority + "/tiny@" + image.manifestDigest
	for _, d := range got.RepoDigests {
		if d == want {
			return nil
		}
	}
	return fmt.Errorf("manifest identity %s absent from %v", want, got.RepoDigests)
}

type dockerLoadResult struct {
	loadResult
	LayerGETs     int     `json:"layer_gets"`
	ConfigGETs    int     `json:"config_gets"`
	CacheHits     float64 `json:"cache_hits"`
	CacheWaitHits float64 `json:"cache_wait_hits"`
	CacheMisses   float64 `json:"cache_misses"`
}

func load4eLogPhase(t *testing.T, r dockerLoadResult, elapsed time.Duration, latencies []time.Duration, layerBytes, configBytes int64, metrics map[string]float64) {
	t.Helper()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) time.Duration {
		i := int(float64(len(latencies)-1)*p + .5)
		return latencies[i]
	}
	t.Logf("load clients=%d phase=%s elapsed=%s latency_min=%s latency_median=%s latency_p95=%s origin_layer_gets=%d origin_config_gets=%d origin_layer_bytes=%d origin_config_bytes=%d cache_hits=%.0f cache_wait_hits=%.0f cache_misses=%.0f peak_rss=%d peak_heap=%d peak_fds=%d peak_goroutines=%d peak_charged=%d peak_reserved=%d peak_fills=%d peak_connections=%d disk_logical=%d disk_physical=%d baseline_fd=%d end_fd=%d baseline_goroutines=%d end_goroutines=%d cache_entries=%.0f cache_bytes=%.0f", r.Clients, r.Phase, elapsed, latencies[0], percentile(.5), percentile(.95), r.LayerGETs, r.ConfigGETs, layerBytes, configBytes, r.CacheHits, r.CacheWaitHits, r.CacheMisses, r.PeakRSSBytes, r.PeakHeapBytes, r.PeakFDs, r.PeakGoroutines, r.PeakCacheChargedBytes, r.PeakCacheReservedBytes, r.PeakCacheFills, r.PeakActiveConnections, r.LogicalDiskBytes, r.PhysicalDiskBytes, r.BaselineFDs, r.EndFDs, r.BaselineGoroutines, r.EndGoroutines, metrics["oci_accelerator_cache_entries"], metrics["oci_accelerator_cache_bytes"])
}

func writeDockerLoadReport(t *testing.T, results []dockerLoadResult) {
	t.Helper()
	path := os.Getenv("OCI_ACCELERATOR_DOCKER_LOAD_REPORT")
	if path == "" || t.Failed() || len(results) != 6 {
		return
	}
	writeLoadJSON(t, path, results)
}
