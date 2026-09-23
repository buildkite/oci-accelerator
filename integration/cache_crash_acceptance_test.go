package integration

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestCacheCrashAcceptance kills the compiled proxy while it owns an active
// cache fill, then proves that a fresh process cleans up and can acquire the
// same store lock and fill the same object.
func TestCacheCrashAcceptance(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_FAULT_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_FAULT_E2E=1 to run the cache crash acceptance test")
	}
	requireCommands(t, "go")

	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	body := bytes.Repeat([]byte("crash-fill-deterministic-body-"), 128*1024)
	prefix := body[:128*1024]
	path := faultBlobPath(body)

	var mu sync.Mutex
	originGETs := 0
	originBytes := int64(0)
	firstPaused := make(chan struct{})
	origin, originPort, originCA := startNamedTLS(t, root, "crash-origin-ca", ip.String(), ip, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		originGETs++
		n := originGETs
		mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Docker-Content-Digest", faultDigest(body))
		if n == 3 { // Authorization still reaches the origin; the warm body does not.
			return
		}
		if n == 1 {
			written, _ := w.Write(prefix)
			mu.Lock()
			originBytes += int64(written)
			mu.Unlock()
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(firstPaused)
			<-r.Context().Done()
			return
		}
		written, _ := w.Write(body)
		mu.Lock()
		originBytes += int64(written)
		mu.Unlock()
	}))
	defer origin.Close()
	authority := fmt.Sprintf("%s:%d", ip, originPort)

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	cacheDir := filepath.Join(root, "cache")
	proxyCA := filepath.Join(root, "proxy-ca")
	config := filepath.Join(root, "proxy.yaml")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 16777216\n  authorization_ttl: 0s\n", proxyPort, authority, proxyCA, cacheDir), 0o600)
	command := fmt.Sprintf("env SSL_CERT_FILE=%s %s --config %s", shell(originCA), shell(bin), shell(config))
	process := startProcess(t, command)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))

	caBefore := mustReadCrashFile(t, filepath.Join(proxyCA, "ca.crt"))
	client := faultProxyClient(t, proxyPort, filepath.Join(proxyCA, "ca.crt"))
	target := "https://" + authority + path
	requestDone := make(chan error, 1)
	go func() {
		resp, err := client.Get(target)
		if err != nil {
			requestDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		requestDone <- err
	}()
	select {
	case <-firstPaused:
	case <-time.After(20 * time.Second):
		t.Fatal("origin did not begin the crash fill")
	}
	waitForNonemptyFillBody(t, cacheDir)

	process.Crash(t)
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("in-flight client unexpectedly succeeded after abrupt proxy kill")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight client did not fail after abrupt proxy kill")
	}
	process = startProcess(t, command)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	caAfter := mustReadCrashFile(t, filepath.Join(proxyCA, "ca.crt"))
	if !bytes.Equal(caBefore, caAfter) {
		t.Fatal("proxy CA changed across crash restart")
	}
	if fills, err := filepath.Glob(filepath.Join(cacheDir, ".fill-*")); err != nil || len(fills) != 0 {
		t.Fatalf("abandoned fills not cleaned after restart: %v (%v)", fills, err)
	}

	client = faultProxyClient(t, proxyPort, filepath.Join(proxyCA, "ca.crt"))
	cold := faultGet(t, client, target)
	warm := faultGet(t, client, target)
	if !bytes.Equal(cold, body) || !bytes.Equal(warm, body) {
		t.Fatalf("restart cache bytes differ: cold=%s warm=%s want=%s", faultDigest(cold), faultDigest(warm), faultDigest(body))
	}
	mu.Lock()
	gets, sent := originGETs, originBytes
	mu.Unlock()
	wantBytes := int64(len(prefix) + len(body))
	if gets != 3 || sent != wantBytes {
		t.Fatalf("origin evidence=%d GET/%d bytes, want 3 GET/%d bytes", gets, sent, wantBytes)
	}
	t.Logf("abrupt_pid_kill=true abandoned_fill_cleaned=true flock_reacquired=true ca_stable=true origin=%d_GET/%d_bytes cold_exact=true warm_headers_only=true", gets, sent)
}

func mustReadCrashFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func waitForNonemptyFillBody(t *testing.T, cacheDir string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(cacheDir, ".fill-*", "body"))
		for _, match := range matches {
			if st, err := os.Stat(match); err == nil && st.Size() > 0 {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("cache fill body did not become nonempty")
}
