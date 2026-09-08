package integration

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCacheENOSPCAcceptance deliberately exhausts a private tmpfs. It is kept
// opt-in because it requires passwordless sudo and Linux mount namespaces.
func TestCacheENOSPCAcceptance(t *testing.T) {
	if os.Getenv("OCI_ACCELERATOR_FAULT_E2E") != "1" {
		t.Skip("set OCI_ACCELERATOR_FAULT_E2E=1 to run the cache fault acceptance test")
	}
	requireCommands(t, "sudo", "unshare", "nsenter", "mount", "dd", "pgrep", "amp")
	run(t, "sudo", "-n", "true")

	root := t.TempDir()
	ip := nonLoopbackIPv4(t)
	faultBody := bytes.Repeat([]byte("fault-transfer-"), 40*1024) // 600 KiB.
	recoveryBody := bytes.Repeat([]byte("recovered-cache-"), 8*1024)
	faultPath := faultBlobPath(faultBody)
	recoveryPath := faultBlobPath(recoveryBody)

	var mu sync.Mutex
	gets := map[string]int{}
	bytesSent := map[string]int64{}
	origin, originPort, originCA := startNamedTLS(t, root, "fault-origin-ca", ip.String(), ip, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gets[r.URL.Path]++
		n := gets[r.URL.Path]
		mu.Unlock()
		var body []byte
		switch r.URL.Path {
		case faultPath:
			body = faultBody
		case recoveryPath:
			body = recoveryBody
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Docker-Content-Digest", faultDigest(body))
		// A warm request proves substitution from cache: the origin supplies valid
		// headers but closes without any representation bytes.
		if r.URL.Path == recoveryPath && n > 1 {
			return
		}
		written, _ := w.Write(body)
		mu.Lock()
		bytesSent[r.URL.Path] += int64(written)
		mu.Unlock()
	}))
	defer origin.Close()
	authority := fmt.Sprintf("%s:%d", ip, originPort)

	bin := filepath.Join(root, "oci-accelerator")
	run(t, "go", "build", "-o", bin, "../cmd/oci-accelerator")
	proxyPort := freePort(t, "127.0.0.1")
	mountpoint := filepath.Join(root, "cacheparent")
	if err := os.Mkdir(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "proxy.yaml")
	proxyCA := filepath.Join(root, "proxy-ca")
	writeFile(t, config, fmt.Sprintf("listen: 127.0.0.1:%d\nregistries: [%q]\nca_dir: %q\ncache:\n  dir: %q\n  max_bytes: 921600\n", proxyPort, authority, proxyCA, filepath.Join(mountpoint, "cache")), 0o600)

	uid, gid := os.Getuid(), os.Getgid()
	command := fmt.Sprintf(`sudo -n unshare --mount --propagation private sh -ceu %s`, shell(fmt.Sprintf(
		`mount -t tmpfs -o size=1m,nr_inodes=1024 fault-cache %s
mkdir %s/cache
chown %d:%d %s %s/cache
chmod 700 %s %s/cache
dd if=/dev/zero of=%s/ballast bs=1024 count=700 status=none
chown %d:%d %s/ballast
sudo -n -u \#%d -g \#%d env SSL_CERT_FILE=%s %s --config %s`,
		shell(mountpoint), shell(mountpoint), uid, gid, shell(mountpoint), shell(mountpoint), shell(mountpoint), shell(mountpoint), shell(mountpoint), uid, gid, shell(mountpoint), uid, gid, shell(originCA), shell(bin), shell(config))))
	name := "fault-cache-" + strings.ToLower(fmt.Sprintf("%x", sha256.Sum256([]byte(root)))[:10])
	t.Cleanup(func() { stopService(t, name) })
	startService(t, name, command)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))

	nsPIDBytes, err := exec.Command("pgrep", "-n", "-f", bin+" --config "+config).Output()
	if err != nil {
		t.Fatalf("locate supervised proxy process: %v", err)
	}
	nsPID := strings.TrimSpace(string(nsPIDBytes))
	ns := func(args ...string) (string, error) {
		base := []string{"-n", "nsenter", "-t", nsPID, "-m", "--"}
		out, err := exec.Command("sudo", append(base, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := ns("findmnt", "-n", "-o", "FSTYPE,SOURCE,SIZE", "--target", mountpoint); err != nil || !strings.Contains(out, "tmpfs") || !strings.Contains(out, "fault-cache") {
		t.Fatalf("cache is not on the private fault tmpfs: %v: %s", err, strings.TrimSpace(out))
	}

	client := faultProxyClient(t, proxyPort, filepath.Join(proxyCA, "ca.crt"))
	got := faultGet(t, client, "https://"+authority+faultPath)
	if !bytes.Equal(got, faultBody) {
		t.Fatalf("fault transfer digest=%s want=%s bytes=%d/%d", faultDigest(got), faultDigest(faultBody), len(got), len(faultBody))
	}
	// Independently demonstrate that the same filesystem is actually at ENOSPC.
	if out, err := ns("dd", "if=/dev/zero", "of="+filepath.Join(mountpoint, "enospc-probe"), "bs=65536", "count=8", "conv=fsync", "status=none"); err == nil || !strings.Contains(strings.ToLower(out), "no space left on device") {
		t.Fatalf("tmpfs probe did not observe ENOSPC: err=%v output=%q", err, strings.TrimSpace(out))
	}
	_, _ = ns("rm", "-f", filepath.Join(mountpoint, "enospc-probe"))
	time.Sleep(300 * time.Millisecond)
	if out, err := ns("find", filepath.Join(mountpoint, "cache"), "-mindepth", "1", "!", "-name", ".lock", "-print"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("failed fill left published or abandoned cache files: %v: %s", err, out)
	}

	if out, err := ns("rm", "-f", filepath.Join(mountpoint, "ballast")); err != nil {
		t.Fatalf("remove owned ballast: %v: %s", err, out)
	}
	cold := faultGet(t, client, "https://"+authority+recoveryPath)
	warm := faultGet(t, client, "https://"+authority+recoveryPath)
	if !bytes.Equal(cold, recoveryBody) || !bytes.Equal(warm, recoveryBody) {
		t.Fatalf("recovered cache returned wrong bytes: cold=%s warm=%s want=%s", faultDigest(cold), faultDigest(warm), faultDigest(recoveryBody))
	}
	mu.Lock()
	faultGets, faultBytes := gets[faultPath], bytesSent[faultPath]
	recoveryGets, recoveryBytes := gets[recoveryPath], bytesSent[recoveryPath]
	mu.Unlock()
	if faultGets != 1 || faultBytes != int64(len(faultBody)) || recoveryGets != 2 || recoveryBytes != int64(len(recoveryBody)) {
		t.Fatalf("origin evidence fault=%d GET/%d bytes recovery=%d GET/%d bytes", faultGets, faultBytes, recoveryGets, recoveryBytes)
	}
	t.Logf("private_tmpfs=1MiB ballast=700KiB ENOSPC=true fault_origin=%d/%d recovery_origin=%d/%d exact_digests=true warm_cache_substitution=true", faultGets, faultBytes, recoveryGets, recoveryBytes)
}

func faultDigest(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }
func faultBlobPath(body []byte) string {
	return "/v2/fault/image/blobs/" + faultDigest(body)
}

func faultProxyClient(t *testing.T, port int, caPath string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("could not parse proxy CA")
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Timeout: 20 * time.Second, Transport: transport}
}

func faultGet(t *testing.T, client *http.Client, target string) []byte {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", target, resp.Status, body)
	}
	return body
}
